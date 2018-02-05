package main

import (
	"flag"
	"log"
	"runtime"

	"github.com/wirepair/gcd"

	"encoding/csv"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"math"
	"time"
	"math/rand"
	"errors"
	"github.com/wirepair/gcd/gcdapi"
	"encoding/json"
	"os/signal"
	"github.com/chromedp/cdproto/page"
)

// TODO: (in decreasing priority)
// click through the popups (embedded page blah says "Could not fetch data")
// track visited decoys and not crawl them multiple times
// handle when links download things (partially remediated by timeout)
// handle HTTP 204 (partially remediated by timeout)
// stop requests to non-decoy before they happen

const maxResourceBufferSize = 16 * 1024 * 1024       // Per-resource buffer size in bytes to use when preserving network payloads (XHRs, etc).
const maxTotalBufferSize = maxResourceBufferSize * 4 // Buffer size in bytes to use when preserving network payloads (XHRs, etc)
const pageLimit = 100 // stop crawling decoy after vising a number of pages

var leafExtensions = []string{
	".jpg",
	".jpeg",
	".png",
	".gif",
	".svg",
	".ico",
	".dat",
}

var chrome_path string
var csv_filename string

func main() {
	switch runtime.GOOS {
	case "darwin":
		flag.StringVar(&chrome_path, "chrome_path",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"Path to Google Chrome binary.")
	case "linux":
		flag.StringVar(&chrome_path, "chrome_path",
			"/usr/bin/google-chrome-stable",
			"Path to Google Chrome binary.")
	}
	flag.StringVar(&csv_filename, "csv_path",
		"./20170516-1131-decoys.txt",
		"Path to input CSV file. Decoys will be parsed from second column.")
	flag.Parse()

	decoys := parseCsvColumn(csv_filename, 1)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	signal.Notify(signalChan, os.Kill)
	prober := NewDittoProber(decoys)
	go func() {
		for sig := range signalChan {
			fmt.Println(sig.String())
			prober.printResults()
			os.Exit(-1)
		}
	}()
	prober.StartProbing()
	prober.printResults()
}

func parseCsvColumn(filename string, column int) []string {
	f, err := os.Open(filename)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	csvLinesColumns, err := csv.NewReader(f).ReadAll()
	if err != nil {
		panic(err)
	}

	hostnames := make([]string, len(csvLinesColumns))

	for i := 0; i < len(csvLinesColumns); i++ {
		// if csv is misformatted, this will panic, which is kinda okay
		hostnames[i] = csvLinesColumns[i][column]
	}
	set_hostnames := make(map[string]struct{})
	for _, host := range hostnames {
		set_hostnames[host] = struct{}{}
	}

	unique_hostnames := make([]string, 0)
	for host := range set_hostnames {
		unique_hostnames = append(unique_hostnames, host)
	}

	return unique_hostnames
}

type DittoProber struct {
	decoys            []string
	debugger          *gcd.Gcd
	stats             map[string]*DecoyStats
	resourceStatsChan chan (*ResourceStats)
	pageStatsChan     chan (*PageStats)
}

func NewDittoProber(decoys []string) *DittoProber {
	dp := &DittoProber{
		decoys:        decoys,
		debugger:      gcd.NewChromeDebugger(),
		stats:         make(map[string]*DecoyStats),
		resourceStatsChan: make(chan *ResourceStats),
		pageStatsChan: make(chan *PageStats),
	}
	return dp
}

func (dp *DittoProber) StartProbing() {
	tmpDir, err := ioutil.TempDir("/tmp/", "ditto-gcache")
	if err != nil {
		log.Fatal(err)
	}

	dp.debugger.StartProcess(chrome_path, tmpDir, "9222")
	defer dp.debugger.ExitProcess()

	done := make(chan struct{})

	// spin up a goroutine that will receive and store page statistics
	go func() {
		for {
			select {
			case resStats := <-dp.resourceStatsChan:
				if _, ok := dp.stats[resStats.hostname]; !ok {
					dp.stats[resStats.hostname] = &DecoyStats{}
				}
				if resStats.isGoodput {
					dp.stats[resStats.hostname].goodput += resStats.size
					dp.stats[resStats.hostname].leafs += 1
				} else {
					dp.stats[resStats.hostname].badput += resStats.size
					dp.stats[resStats.hostname].nonleafs += 1
				}
			case pageStat := <-dp.pageStatsChan:
				if _, ok := dp.stats[pageStat.hostname]; !ok {
					dp.stats[pageStat.hostname] = &DecoyStats{}
				}
				dp.stats[pageStat.hostname].pages_total += 1
				if pageStat.final {
					dp.stats[pageStat.hostname].final_depths =
						append(dp.stats[pageStat.hostname].final_depths, pageStat.depth)
				}
			case <-done:
				return
			}
		}
	}()

	for _, decoy := range dp.decoys {
		dp.probeDecoy(decoy)
	}
	done <- struct{}{}

}

func (dp *DittoProber) probeDecoy(decoyUrl string) {
	currNavigatedURL, err := url.Parse(decoyUrl)
	if err != nil {
		log.Printf("Error parsing decoy url %s: %s\n", decoyUrl, err)
		return
	}
	target, err := dp.debugger.NewTab()
	defer dp.debugger.CloseTab(target)
	if err != nil {
		log.Printf("Error creating new empty tab: %s\n", err)
		return
	}
	if _, err := target.Network.Enable(maxTotalBufferSize, maxResourceBufferSize); err != nil {
		log.Printf("Error enabling network tracking: %s\n", err)
		return
	}
	if _, err := target.Page.Enable(); err != nil {
		log.Printf("Error enabling page domain notifications: %s\n", err)
		return
	}

	// use a WaitGroup to signal full page load
	var decoyMainPageLoadWG sync.WaitGroup
	decoyLeafPageLoadChan := make(chan struct{})
	decoyMainPageLoadWG.Add(1)

	isGoodput := func(targetUrl string) bool {
		responseUrl, err := url.Parse(targetUrl)
		if err != nil {
			log.Printf("Error parsing response URL (%s): %s\n",
				targetUrl, err)
			return false
		}
		// not a leaf if resource was fetched from non-decoy host
		if decoyUrl != responseUrl.Host {
			return false
		}

		// check if url extension is one of extensions allocated for leafs
		extension := filepath.Ext(targetUrl)
		for _, le := range leafExtensions {
			if extension == le {
				return true
			}
		}
		return false
	}

	visited_links := NewSafeStringIntMap()
	links_to_visit := NewSafeStringIntMap()
	cached_urls := NewSafeStringSet()
	final_hostpaths := NewSafeStringSet()

	target.Subscribe("Security.certificateError", func(target *gcd.ChromeTarget, event []byte) {
		// TODO: this didn't seem to actually cancel the events
		eventObj := gcdapi.SecurityCertificateErrorEvent{}
		json.Unmarshal(event, &eventObj)
		fmt.Println("[INFO] Handling ceriticate error ", eventObj)
		if err != nil {
			decoyMainPageLoadWG.Done()
			return
		}
		target.Security.HandleCertificateError(eventObj.Params.EventId, "cancel")
	})

	target.Subscribe("Network.responseReceived", func(target *gcd.ChromeTarget, event []byte) {
		eventObj := gcdapi.NetworkResponseReceivedEvent{}
		err := json.Unmarshal(event, &eventObj)
		if err != nil {
			return
		}
		urlObj, err := url.Parse(eventObj.Params.Response.Url)
		if err != nil {
			visited_links.set(eventObj.Params.Response.Url, 0)
		} else {
			visited_links.set(urlObj.Host + urlObj.Host, 0)
		}
	})

	//subscribe to page load
	var mainPageLoadOnce sync.Once
	target.Subscribe("Page.loadEventFired", func(target *gcd.ChromeTarget, event []byte) {
		mainPage := false
		mainPageLoadOnce.Do(func() {
			mainPage = true
		})
		defer func() {
			// potential deadlock if this event does not fire on
			// failed load, but this didn't seem to happen in practice yet
			if mainPage {
				decoyMainPageLoadWG.Done()
			} else {
				decoyLeafPageLoadChan <- struct{}{}
			}
		}()
		var err error

		rand.Seed(time.Now().UnixNano())

		sleepDuration := time.Duration(math.Abs(rand.NormFloat64() * 2) * float64(time.Second))
		time.Sleep(sleepDuration)

		dom := target.DOM
		root, err := dom.GetDocument(-1, true)
		if err != nil {
			fmt.Println("Error getting root: ", err)
			return
		}

		links, err := dom.QuerySelectorAll(root.NodeId, "a")
		if err != nil {
			fmt.Println("Error getting links: ", err)
			return
		}
		docUrl, err := url.Parse(root.DocumentURL)
		if err != nil {
			fmt.Printf("Error parsing url %s : %s\n", root.DocumentURL, err)
		}
		if mainPage {
			fmt.Printf("[INFO] Decoy main page redirect detected: %s -> %s\n",
				decoyUrl, docUrl.Host)
			decoyUrl = docUrl.Host
			visited_links.set(docUrl.Host + docUrl.Path, 0)
			visited_links.set(docUrl.Host + docUrl.Path + "/", 0)
		} else if docUrl.Host != decoyUrl {
			fmt.Printf("[INFO] Redirect detected. Decoy: %s, docUrl.Host: %s\n",
				decoyUrl, docUrl.Host)
			visited_links.set(docUrl.Host + docUrl.Path, 0)
			return
		}

		depth, ok := visited_links.get(docUrl.Host + docUrl.Path)
		if !ok {
			fmt.Printf("Error: url %s not in visited_links!\n", docUrl.Host + docUrl.Path)
			depth = 0
		}

		for _, l := range links {
			attributes, err := dom.GetAttributes(l)
			if err != nil {
				fmt.Println(" error getting attributes: ", err)
				return
			}

			attributesMap := make(map[string]string)
			for i := 0; i < len(attributes); i += 2 {
				attributesMap[attributes[i]] = attributes[i + 1]
			}

			if _, hasHref := attributesMap["href"]; !hasHref {
				continue
			}

			currUrl, err := url.Parse(attributesMap["href"])
			if err != nil {
				fmt.Printf("Could not parse url %s: %v\n", attributesMap["href"], err)
				continue
			}
			currUrl.Scheme = "https"
			if currUrl.Hostname() == "" {
				currUrl = currNavigatedURL.ResolveReference(currUrl)
				currUrl.Host = decoyUrl
			}
			currUrl.Fragment = ""
			if currUrl.Hostname() != decoyUrl {
				continue
			}
			if !visited_links.contains(currUrl.Host + currUrl.Path) {
				links_to_visit.set(currUrl.String(), depth + 1)
			}
		}
		if len(links) == 0 || links_to_visit.size() == 0 {
			final_hostpaths.add(docUrl.Host + docUrl.Path)
		}

	})

	page.SetDownloadBehavior(page.SetDownloadBehaviorBehaviorDeny)
	fmt.Println("Decoy url:", decoyUrl)
	visited_links.set(decoyUrl, 0)
	visited_links.set(decoyUrl + "/", 0)
	_, errStr, err := target.Page.Navigate("https://" + decoyUrl, "", "") // navigate
	if errStr != "" {
		log.Printf("Error string navigating: %s\n", errStr)
		return
	}
	if err != nil {
		log.Printf("Error navigating: %s\n", err)
		return
	}

	decoyMainPageLoadWG.Wait()

	pfrt, err := target.Page.GetResourceTree()
	if err != nil {
		log.Printf("target.Page.GetResourceTree() error: %s\n", err)
		return
	}
	for _, r := range pfrt.Resources {
		stats := &ResourceStats{
			hostname:  decoyUrl,
			isGoodput: isGoodput(r.Url),
			size:      int(r.ContentSize),
		}
		dp.resourceStatsChan <- stats
	}

	mainPageStatPushed := false
	pagesVisited := 0

	for pagesVisited < pageLimit {
		link, depth, err := links_to_visit.popAny()
		if err != nil {
			break
		}
		linkUrlObj, err := url.Parse(link)
		if err != nil {
			log.Printf("Error parsing decoy url %s: %s\n", decoyUrl, err)
			visited_links.set(link, depth)
			continue
		}
		linkUrlObj.Scheme = "https"
		visited_links.set(linkUrlObj.Host + linkUrlObj.Path, depth)
		if !mainPageStatPushed {
			mainPageStatPushed = true
			dp.pageStatsChan <- &PageStats{depth: 0, hostname: decoyUrl, final: (err != nil)}
		}
		fmt.Println(" following link to ", linkUrlObj.String())
		pagesVisited += 1
		_, _, err = target.Page.Navigate(linkUrlObj.String(), "", "")
		if err != nil {
			fmt.Println(" error navigating to ", link, ": ", err)
		} else {
			timeout := time.After(20 * time.Second)
			select {
			case <-timeout:
				fmt.Println(" timed out navigating to", linkUrlObj.String())
				target.Page.StopLoading()
				continue
			case <-decoyLeafPageLoadChan:
			}

			pfrt, err := target.Page.GetResourceTree()
			if err != nil {
				log.Printf("target.Page.GetResourceTree() error: %s\n", err)
				continue
			}
			for _, r := range pfrt.Resources {
				if !cached_urls.contains(r.Url) {
					stats := &ResourceStats{
						hostname:  decoyUrl,
						isGoodput: isGoodput(r.Url),
						size:      int(r.ContentSize),
					}
					dp.resourceStatsChan <- stats
				}
			}
			pageStat := &PageStats{depth:depth,
				hostname: decoyUrl,
				final: final_hostpaths.contains(linkUrlObj.Host + linkUrlObj.Path)}
			dp.pageStatsChan <- pageStat
		}
	}
}

func (dp *DittoProber) printResults() {
	for k, v := range dp.stats {
		allput := v.goodput + v.badput
		ratio := float64(0)
		if allput != 0 {
			ratio = float64(v.goodput) / float64(allput) * 100
		}
		fmt.Printf("[%s] total_bytes: %v goodput_bytes: %v ratio: %v%% " +
			" pages: %v" +
			" leafs: %v non-leafs: %v" +
			" depths: %v\n",
			k, allput, v.goodput, ratio,
			v.pages_total,
			v.leafs, v.nonleafs,
			v.final_depths)
	}
}

type DecoyStats struct {
	goodput, badput int
	leafs, nonleafs int
	pages_total     int
	final_depths    []int
}

type PageStats struct {
	hostname string
	depth    int
	final    bool
}

type ResourceStats struct {
	hostname  string
	isGoodput bool
	size      int
}

type SafeStringSet struct {
	sync.Mutex
	strings map[string]struct{}
}

func NewSafeStringSet() *SafeStringSet {
	s := SafeStringSet{
		strings: make(map[string]struct{}),
	}
	return &s
}

func (s *SafeStringSet) add(str string) {
	s.Lock()
	s.strings[str] = struct{}{}
	s.Unlock()
}

func (s *SafeStringSet) size() int {
	s.Lock()
	defer s.Unlock()
	return len(s.strings)
}

func (s *SafeStringSet) contains(str string) bool {
	s.Lock()
	_, contains := s.strings[str]
	s.Unlock()
	return contains
}

func (s *SafeStringSet) delete(str string) {
	s.Lock()
	delete(s.strings, str)
	s.Unlock()
}

func (s *SafeStringSet) popAny() (string, error) {
	s.Lock()
	defer s.Unlock()
	for str := range s.strings {
		delete(s.strings, str)
		return str, nil
	}
	return "", errors.New("No elements in set")
}

type SafeStringIntMap struct {
	sync.Mutex
	m map[string]int
}

func NewSafeStringIntMap() *SafeStringIntMap {
	s := SafeStringIntMap{
		m: make(map[string]int),
	}
	return &s
}

func (s *SafeStringIntMap) set(k string, v int) {
	s.Lock()
	s.m[k] = v
	s.Unlock()
}

func (s *SafeStringIntMap) size() int {
	s.Lock()
	defer s.Unlock()
	return len(s.m)
}

func (s *SafeStringIntMap) contains(str string) bool {
	s.Lock()
	_, contains := s.m[str]
	s.Unlock()
	return contains
}

func (s *SafeStringIntMap) delete(str string) {
	s.Lock()
	delete(s.m, str)
	s.Unlock()
}

func (s *SafeStringIntMap) get(str string) (int, bool) {
	s.Lock()
	value, ok := s.m[str]
	s.Unlock()
	return value, ok
}

func (s *SafeStringIntMap) popAny() (string, int, error) {
	s.Lock()
	defer s.Unlock()
	for k, v := range s.m {
		delete(s.m, k)
		return k, v, nil
	}
	return "", 0, errors.New("No elements in set")
}
