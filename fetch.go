// Copyright 2014 Martin Angers and Contributors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fetchbot

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
    "math/rand"

	"github.com/temoto/robotstxt-go"
)

var (
	// ErrEmptyHost is returned if a command to be enqueued has an URL with an empty host.
	ErrEmptyHost = errors.New("fetchbot: invalid empty host")

	// ErrDisallowed is returned when the requested URL is disallowed by the robots.txt
	// policy.
	ErrDisallowed = errors.New("fetchbot: disallowed by robots.txt")

	// ErrQueueClosed is returned when a Send call is made on a closed Queue.
	ErrQueueClosed = errors.New("fetchbot: send on a closed queue")
)

// Parse the robots.txt relative path a single time at startup, this can't
// return an error.
var robotsTxtParsedPath, _ = url.Parse("/robots.txt")

const (
	// DefaultCrawlDelay represents the delay to use if there is no robots.txt
	// specified delay.
	DefaultCrawlDelay = 0 * time.Second

	// DefaultUserAgent is the default user agent string.
	DefaultUserAgent = "Fetchbot (https://github.com/PuerkitoBio/fetchbot)"

	// DefaultWorkerIdleTTL is the default time-to-live of an idle host worker goroutine.
	// If no URL is sent for a given host within this duration, this host's goroutine
	// is disposed of.
	DefaultWorkerIdleTTL = 30 * time.Second
)

// Doer defines the method required to use a type as HttpClient.
// The net/*http.Client type satisfies this interface.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// A Fetcher defines the parameters for running a web crawler.
type Fetcher struct {
	// The Handler to be called for each request. All successfully enqueued requests
	// produce a Handler call.
	Handler Handler

	// DisablePoliteness disables fetching and using the robots.txt policies of
	// hosts.
	DisablePoliteness bool

	// Default delay to use between requests to a same host if there is no robots.txt
	// crawl delay or if DisablePoliteness is true.
	CrawlDelay time.Duration

	// The *http.Client to use for the requests. If nil, defaults to the net/http
	// package's default client. Should be HTTPClient to comply with go lint, but
	// this is a breaking change, won't fix.
	HttpClient Doer

	// The user-agent string to use for robots.txt validation and URL fetching.
	UserAgent string

	// The time a host-dedicated worker goroutine can stay idle, with no Command to enqueue,
	// before it is stopped and cleared from memory.
	WorkerIdleTTL time.Duration

	// AutoClose makes the fetcher close its queue automatically once the number
	// of hosts reach 0. A host is removed once it has been idle for WorkerIdleTTL
	// duration.
	AutoClose bool

	// q holds the Queue to send data to the fetcher and optionally close (stop) it.
	q *Queue
	// dbg is a channel used to push debug information.
	dbgmu     sync.Mutex
	dbg       chan *DebugInfo
	debugging bool

	// hosts maps the host names to its dedicated requests channel, and mu protects
	// concurrent access to the hosts field.
	mu    sync.Mutex
	hosts map[string]chan Command
}

// The DebugInfo holds information to introspect the Fetcher's state.
type DebugInfo struct {
	NumHosts int
}

// New returns an initialized Fetcher.
func New(h Handler) *Fetcher {
	return &Fetcher{
		Handler:       h,
		CrawlDelay:    DefaultCrawlDelay,
		HttpClient:    http.DefaultClient,
		UserAgent:     DefaultUserAgent,
		WorkerIdleTTL: DefaultWorkerIdleTTL,
		dbg:           make(chan *DebugInfo, 1),
	}
}

// Queue offers methods to send Commands to the Fetcher, and to Stop the crawling process.
// It is safe to use from concurrent goroutines.
type Queue struct {
	ch chan Command

	// signal channels
	closed, cancelled, done chan struct{}

	wg sync.WaitGroup
}

// Close closes the Queue so that no more Commands can be sent. It blocks until
// the Fetcher drains all pending commands. After the call, the Fetcher is stopped.
// Attempts to enqueue new URLs after Close has been called will always result in
// a ErrQueueClosed error.
func (q *Queue) Close() error {
	// Make sure it is not already closed, as this is a run-time panic
	select {
	case <-q.closed:
		// Already closed, no-op
		return nil
	default:
		// Close the signal-channel
		close(q.closed)
		// Send a nil Command to make sure the processQueue method sees the close signal.
		q.ch <- nil
		// Wait for the Fetcher to drain.
		q.wg.Wait()
		// Unblock any callers waiting on q.Block
		close(q.done)
		return nil
	}
}

// Block blocks the current goroutine until the Queue is closed and all pending
// commands are drained.
func (q *Queue) Block() {
	<-q.done
}

// Done returns a channel that is closed when the Queue is closed (either
// via Close or Cancel). Multiple calls always return the same channel.
func (q *Queue) Done() <-chan struct{} {
	return q.done
}

// Cancel closes the Queue and drains the pending commands without processing
// them, allowing for a fast "stop immediately"-ish operation.
func (q *Queue) Cancel() error {
	select {
	case <-q.cancelled:
		// already cancelled, no-op
		return nil
	default:
		// mark the queue as cancelled
		close(q.cancelled)
		// Close the Queue, that will wait for pending commands to drain
		// will unblock any callers waiting on q.Block
		return q.Close()
	}
}

// Send enqueues a Command into the Fetcher. If the Queue has been closed, it
// returns ErrQueueClosed. The Command's URL must have a Host.
func (q *Queue) Send(c Command) error {
	if c == nil {
		return ErrEmptyHost
	}
	if u := c.URL(); u == nil || u.Host == "" {
		return ErrEmptyHost
	}
	select {
	case <-q.closed:
		return ErrQueueClosed
	default:
		q.ch <- c
	}
	return nil
}

// SendString enqueues a method and some URL strings into the Fetcher. It returns an error
// if the URL string cannot be parsed, or if the Queue has been closed.
// The first return value is the number of URLs successfully enqueued.
func (q *Queue) SendString(method string, rawurl ...string) (int, error) {
	return q.sendWithMethod(method, rawurl)
}

// SendStringHead enqueues the URL strings to be fetched with a HEAD method.
// It returns an error if the URL string cannot be parsed, or if the Queue has been closed.
// The first return value is the number of URLs successfully enqueued.
func (q *Queue) SendStringHead(rawurl ...string) (int, error) {
	return q.sendWithMethod("HEAD", rawurl)
}

// SendStringGet enqueues the URL strings to be fetched with a GET method.
// It returns an error if the URL string cannot be parsed, or if the Queue has been closed.
// The first return value is the number of URLs successfully enqueued.
func (q *Queue) SendStringGet(rawurl ...string) (int, error) {
	return q.sendWithMethod("GET", rawurl)
}

// Parses the URL strings and enqueues them as *Cmd. It returns the number of URLs
// successfully enqueued, and an error if the URL string cannot be parsed or
// the Queue has been closed.
func (q *Queue) sendWithMethod(method string, rawurl []string) (int, error) {
	for i, v := range rawurl {
		parsed, err := url.Parse(v)
		if err != nil {
			return i, err
		}
		if err := q.Send(&Cmd{U: parsed, M: method}); err != nil {
			return i, err
		}
	}
	return len(rawurl), nil
}

// Start starts the Fetcher, and returns the Queue to use to send Commands to be fetched.
func (f *Fetcher) Start() *Queue {
	f.hosts = make(map[string]chan Command)

	f.q = &Queue{
		ch:        make(chan Command, 1),
		closed:    make(chan struct{}),
		cancelled: make(chan struct{}),
		done:      make(chan struct{}),
	}

	// Start the one and only queue processing goroutine.
	f.q.wg.Add(1)
	go f.processQueue()

	return f.q
}

// Debug returns the channel to use to receive the debugging information. It is not intended
// to be used by package users.
func (f *Fetcher) Debug() <-chan *DebugInfo {
	f.dbgmu.Lock()
	defer f.dbgmu.Unlock()
	f.debugging = true
	return f.dbg
}

// processQueue runs the queue in its own goroutine.
func (f *Fetcher) processQueue() {
loop:
	for v := range f.q.ch {
		if v == nil {
			// Special case, when the Queue is closed, a nil command is sent, use this
			// indicator to check for the closed signal, instead of looking on every loop.
			select {
			case <-f.q.closed:
				// Close signal, exit loop
				break loop
			default:
				// Keep going
			}
		}
		select {
		case <-f.q.cancelled:
			// queue got cancelled, drain
			continue
		default:
			// go on
		}

		// Get the URL to enqueue
		u := v.URL()

		// Check if a channel is already started for this host
		f.mu.Lock()
		in, ok := f.hosts[u.Host]
		if !ok {
			// Start a new channel and goroutine for this host.

			var rob *url.URL
			if !f.DisablePoliteness {
				// Must send the robots.txt request.
				rob = u.ResolveReference(robotsTxtParsedPath)
			}

			// Create the infinite queue: the in channel to send on, and the out channel
			// to read from in the host's goroutine, and add to the hosts map
			var out chan Command
			in, out = make(chan Command, 1), make(chan Command, 1)
			f.hosts[u.Host] = in
			f.mu.Unlock()
			f.q.wg.Add(1)
			// Start the infinite queue goroutine for this host
			go sliceIQ(in, out)
			// Start the working goroutine for this host
			go f.processChan(out, u.Host)

			if !f.DisablePoliteness {
				// Enqueue the robots.txt request first.
				in <- robotCommand{&Cmd{U: rob, M: "GET"}}
			}
		} else {
			f.mu.Unlock()
		}
		// Send the request
		in <- v

		// Send debug info, but do not block if full
		f.dbgmu.Lock()
		if f.debugging {
			f.mu.Lock()
			select {
			case f.dbg <- &DebugInfo{len(f.hosts)}:
			default:
			}
			f.mu.Unlock()
		}
		f.dbgmu.Unlock()
	}

	// Close all host channels now that it is impossible to send on those. Those are the `in`
	// channels of the infinite queue. It will then drain any pending events, triggering the
	// handlers for each in the worker goro, and then the infinite queue goro will terminate
	// and close the `out` channel, which in turn will terminate the worker goro.
	f.mu.Lock()
	for _, ch := range f.hosts {
		close(ch)
	}
	f.hosts = make(map[string]chan Command)
	f.mu.Unlock()

	f.q.wg.Done()
}

// Goroutine for a host's worker, processing requests for all its URLs.
func (f *Fetcher) processChan(ch <-chan Command, hostKey string) {
	var (
		agent *robotstxt.Group
		wait  <-chan time.Time
		ttl   <-chan time.Time
		delay = f.CrawlDelay
	)

loop:
	for {
		select {
		case <-f.q.cancelled:
			break loop
		case v, ok := <-ch:
			if !ok {
				// Terminate this goroutine, channel is closed
				break loop
			}

			// Wait for the prescribed delay
			if wait != nil {
				<-wait
			}

			// was it cancelled during the wait? check again
			select {
			case <-f.q.cancelled:
				break loop
			default:
				// go on
			}

			switch r, ok := v.(robotCommand); {
			case ok:
				// This is the robots.txt request
				agent = f.getRobotAgent(r)
				// Initialize the crawl delay
				if agent != nil && agent.CrawlDelay > 0 {
					delay = agent.CrawlDelay
				} else {
                    delay = randomCrawlDelay()
                }
				wait = time.After(delay)

			case agent == nil || agent.Test(v.URL().Path):
				// Path allowed, process the request
				res, err := f.doRequest(v)
				f.visit(v, res, err)
				// No delay on error - the remote host was not reached
				if err == nil {
                    
					wait = time.After(delay)
				} else {
					wait = nil
				}

			default:
				// Path disallowed by robots.txt
				f.visit(v, nil, ErrDisallowed)
				wait = nil
			}
			// Every time a command is received, reset the ttl channel
			ttl = time.After(f.WorkerIdleTTL)

		case <-ttl:
			// Worker has been idle for WorkerIdleTTL, terminate it
			f.mu.Lock()
			inch, ok := f.hosts[hostKey]
			delete(f.hosts, hostKey)

			// Close the queue if AutoClose is set and there are no more hosts.
			if f.AutoClose && len(f.hosts) == 0 {
				go f.q.Close()
			}
			f.mu.Unlock()
			if ok {
				close(inch)
			}
			break loop
		}
	}

	// need to drain ch until it is closed, to prevent the producer goroutine
	// from leaking.
	for _ = range ch {
	}

	f.q.wg.Done()
}

// Get the robots.txt User-Agent-specific group.
func (f *Fetcher) getRobotAgent(r robotCommand) *robotstxt.Group {
    
    // This is where the logic for proxies will go.
    // TODO Create a function that will get a proxy, validate it, and return it.
    // TODO If the proxy is not valid, it must be marked (possibly for the domain) as bad.
    
    // selectedProxy := RandomString(proxyOptions)
	// proxyUrl, _ := url.Parse(selectedProxy)
    //
     
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyUrl)}}
 
	res, err := f.doRequest(r)
	if err != nil {
		// TODO: Ignore robots.txt request error?
		fmt.Fprintf(os.Stderr, "fetchbot: error fetching robots.txt: %s\n", err)
		return nil
	}
	if res.Body != nil {
		defer res.Body.Close()
	}
	robData, err := robotstxt.FromResponse(res)
	if err != nil {
		// TODO : Ignore robots.txt parse error?
		fmt.Fprintf(os.Stderr, "fetchbot: error parsing robots.txt: %s\n", err)
		return nil
	}
	return robData.FindGroup(f.UserAgent)
}

// Call the Handler for this Command. Closes the response's body.
func (f *Fetcher) visit(cmd Command, res *http.Response, err error) {
	if res != nil && res.Body != nil {
		defer res.Body.Close()
	}
	// if the Command implements Handler, call that handler, otherwise
	// dispatch to the Fetcher's Handler.
	if h, ok := cmd.(Handler); ok {
		h.Handle(&Context{Cmd: cmd, Q: f.q}, res, err)
		return
	}
	f.Handler.Handle(&Context{Cmd: cmd, Q: f.q}, res, err)
}

// Prepare and execute the request for this Command.
func (f *Fetcher) doRequest(cmd Command) (*http.Response, error) {
	req, err := http.NewRequest(cmd.Method(), cmd.URL().String(), nil)
	if err != nil {
		return nil, err
	}
	// If the Command implements some other recognized interfaces, set
	// the request accordingly (see cmd.go for the list of interfaces).
	// First, the Header values.
	if hd, ok := cmd.(HeaderProvider); ok {
		for k, v := range hd.Header() {
			req.Header[k] = v
		}
	}
	// BasicAuth has higher priority than an Authorization header set by
	// a HeaderProvider.
	if ba, ok := cmd.(BasicAuthProvider); ok {
		req.SetBasicAuth(ba.BasicAuth())
	}
	// Cookies are added to the request, even if some cookies were set
	// by a HeaderProvider.
	if ck, ok := cmd.(CookiesProvider); ok {
		for _, c := range ck.Cookies() {
			req.AddCookie(c)
		}
	}
	// For the body of the request, ReaderProvider has higher priority
	// than ValuesProvider.
	if rd, ok := cmd.(ReaderProvider); ok {
		rdr := rd.Reader()
		rc, ok := rdr.(io.ReadCloser)
		if !ok {
			rc = ioutil.NopCloser(rdr)
		}
		req.Body = rc
	} else if val, ok := cmd.(ValuesProvider); ok {
		v := val.Values()
		req.Body = ioutil.NopCloser(strings.NewReader(v.Encode()))
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	// If there was no User-Agent implicitly set by the HeaderProvider,
	// set it to the default value.
	// if req.Header.Get("User-Agent") == "" {
	// 	req.Header.Set("User-Agent", f.UserAgent)
	// }
    
    // Setting random user agent.
    req.Header.Set("User-Agent", randomUserAgent())
    
	// Do the request.
	res, err := f.HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Pseudo random user agent returned as a string.
func randomUserAgent() string {
    userAgents := []string{
        // Samsung Galaxy S8
        "Mozilla/5.0 (Linux; Android 7.0; SM-G892A Build/NRD90M; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/60.0.3112.107 Mobile Safari/537.36",
        // Samsung Galaxy S7
        "Mozilla/5.0 (Linux; Android 7.0; SM-G930VC Build/NRD90M; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/58.0.3029.83 Mobile Safari/537.36",
        // Samsung Galaxy S7 Edge
        "Mozilla/5.0 (Linux; Android 6.0.1; SM-G935S Build/MMB29K; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/55.0.2883.91 Mobile Safari/537.36",
        // Samsung Galaxy S6
        "Mozilla/5.0 (Linux; Android 6.0.1; SM-G920V Build/MMB29K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/52.0.2743.98 Mobile Safari/537.36",
        // Samsung Galaxy S6 Edge Plus
        "Mozilla/5.0 (Linux; Android 5.1.1; SM-G928X Build/LMY47X) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/47.0.2526.83 Mobile Safari/537.36",
        // Nexus 6P
        "Mozilla/5.0 (Linux; Android 6.0.1; Nexus 6P Build/MMB29P) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/47.0.2526.83 Mobile Safari/537.36",
        // Sony Xperia XZ
        "Mozilla/5.0 (Linux; Android 7.1.1; G8231 Build/41.2.A.0.219; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/59.0.3071.125 Mobile Safari/537.36",
        // Sony Xperia Z5
        "Mozilla/5.0 (Linux; Android 6.0.1; E6653 Build/32.2.A.0.253) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/52.0.2743.98 Mobile Safari/537.36",
        // HTC One X10
        "Mozilla/5.0 (Linux; Android 6.0; HTC One X10 Build/MRA58K; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/61.0.3163.98 Mobile Safari/537.36",
        // HTC One M9
        "Mozilla/5.0 (Linux; Android 6.0; HTC One M9 Build/MRA58K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/52.0.2743.98 Mobile Safari/537.36",

        // Apple iPhone X
        "Mozilla/5.0 (iPhone; CPU iPhone OS 11_0 like Mac OS X) AppleWebKit/604.1.38 (KHTML, like Gecko) Version/11.0 Mobile/15A372 Safari/604.1",
        // Apple iPhone 8
        "Mozilla/5.0 (iPhone; CPU iPhone OS 11_0 like Mac OS X) AppleWebKit/604.1.34 (KHTML, like Gecko) Version/11.0 Mobile/15A5341f Safari/604.1",
        // Apple iPhone 8 Plus
        "Mozilla/5.0 (iPhone; CPU iPhone OS 11_0 like Mac OS X) AppleWebKit/604.1.38 (KHTML, like Gecko) Version/11.0 Mobile/15A5370a Safari/604.1",
        // Apple iPhone 7
        "Mozilla/5.0 (iPhone9,3; U; CPU iPhone OS 10_0_1 like Mac OS X) AppleWebKit/602.1.50 (KHTML, like Gecko) Version/10.0 Mobile/14A403 Safari/602.1",
        // Apple iPhone 7 Plus
        "Mozilla/5.0 (iPhone9,4; U; CPU iPhone OS 10_0_1 like Mac OS X) AppleWebKit/602.1.50 (KHTML, like Gecko) Version/10.0 Mobile/14A403 Safari/602.1",
        // Apple iPhone 6
        "Mozilla/5.0 (Apple-iPhone7C2/1202.466; U; CPU like Mac OS X; en) AppleWebKit/420+ (KHTML, like Gecko) Version/3.0 Mobile/1A543 Safari/419.3",

        // Lumia 650
        "Mozilla/5.0 (Windows Phone 10.0; Android 6.0.1; Microsoft; RM-1152) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/52.0.2743.116 Mobile Safari/537.36 Edge/15.15254",
        // Microsoft Lumia 550
        "Mozilla/5.0 (Windows Phone 10.0; Android 4.2.1; Microsoft; RM-1127_16056) AppleWebKit/537.36(KHTML, like Gecko) Chrome/42.0.2311.135 Mobile Safari/537.36 Edge/12.10536",
        // Microsoft Lumia 950
        "Mozilla/5.0 (Windows Phone 10.0; Android 4.2.1; Microsoft; Lumia 950) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/46.0.2486.0 Mobile Safari/537.36 Edge/13.10586",

        // Google Pixel C
        "Mozilla/5.0 (Linux; Android 7.0; Pixel C Build/NRD90M; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/52.0.2743.98 Safari/537.36",
        // Sony Xperia Z4 Tablet
        "Mozilla/5.0 (Linux; Android 6.0.1; SGP771 Build/32.2.A.0.253; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/52.0.2743.98 Safari/537.36",
        // Nvidia Shield Tablet K1
        "Mozilla/5.0 (Linux; Android 6.0.1; SHIELD Tablet K1 Build/MRA58K; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/55.0.2883.91 Safari/537.36",
        // Samsung Galaxy Tab S3
        "Mozilla/5.0 (Linux; Android 7.0; SM-T827R4 Build/NRD90M) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/60.0.3112.116 Safari/537.36",
        // Samsung Galaxy Tab A
        "Mozilla/5.0 (Linux; Android 5.0.2; SAMSUNG SM-T550 Build/LRX22G) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/3.3 Chrome/38.0.2125.102 Safari/537.36",
        // Amazon Kindle Fire HDX 7
        "Mozilla/5.0 (Linux; Android 4.4.3; KFTHWI Build/KTU84M) AppleWebKit/537.36 (KHTML, like Gecko) Silk/47.1.79 like Chrome/47.0.2526.80 Safari/537.36",
        // LG G Pad 7.0
        "Mozilla/5.0 (Linux; Android 5.0.2; LG-V410/V41020c Build/LRX22G) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/34.0.1847.118 Safari/537.36",

        // Windows 10-based PC using Edge browser
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/42.0.2311.135 Safari/537.36 Edge/12.246",
        // Chrome OS-based laptop using Chrome browser (Chromebook)
        "Mozilla/5.0 (X11; CrOS x86_64 8172.45.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/51.0.2704.64 Safari/537.36",
        // Mac OS X-based computer using a Safari browser
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_11_2) AppleWebKit/601.3.9 (KHTML, like Gecko) Version/9.0.2 Safari/601.3.9",
        // Windows 7-based PC using a Chrome browser
        "Mozilla/5.0 (Windows NT 6.1; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/47.0.2526.111 Safari/537.36",
        // Linux-based PC using a Firefox browser
        "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:15.0) Gecko/20100101 Firefox/15.0.1",

        // Chromecast
        "Mozilla/5.0 (CrKey armv7l 1.5.16041) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/31.0.1650.0 Safari/537.36",
        // Roku Ultra
        "Roku4640X/DVP-7.70 (297.70E04154A)",
        // Minix NEO X5
        "Mozilla/5.0 (Linux; U; Android 4.2.2; he-il; NEO-X5-116A Build/JDQ39) AppleWebKit/534.30 (KHTML, like Gecko) Version/4.0 Safari/534.30",
        // Amazon 4K Fire TV
        "Mozilla/5.0 (Linux; Android 5.1; AFTS Build/LMY47O) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/41.99900.2250.0242 Safari/537.36",
        // Google Nexus Player
        "Dalvik/2.1.0 (Linux; U; Android 6.0.1; Nexus Player Build/MMB29T)",
        // Apple TV 5th Gen 4K
        "AppleTV6,2/11.1",
        // Apple TV 4th Gen
        "AppleTV5,3/9.1.1",

        // Nintendo Wii U
        "Mozilla/5.0 (Nintendo WiiU) AppleWebKit/536.30 (KHTML, like Gecko) NX/3.0.4.2.12 NintendoBrowser/4.3.1.11264.US",
        // Xbox One S
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64; XBOX_ONE_ED) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/51.0.2704.79 Safari/537.36 Edge/14.14393",
        // Xbox One
        "Mozilla/5.0 (Windows Phone 10.0; Android 4.2.1; Xbox; Xbox One) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/46.0.2486.0 Mobile Safari/537.36 Edge/13.10586",
        // Playstation 4
        "Mozilla/5.0 (PlayStation 4 3.11) AppleWebKit/537.73 (KHTML, like Gecko)",
        // Playstation Vita
        "Mozilla/5.0 (PlayStation Vita 3.61) AppleWebKit/537.73 (KHTML, like Gecko) Silk/3.2",
        // Nintendo 3DS
        "Mozilla/5.0 (Nintendo 3DS; U; ; en) Version/1.7412.EU",
    }
	rand.Seed(time.Now().Unix())
	randNum := rand.Int() % len(userAgents)
	return userAgents[randNum]
}

// Simple random time delay for crawler (0-4 seconds).
func randomCrawlDelay() time.Duration {
    rand.Seed(time.Now().Unix())
    return time.Duration(rand.Intn(5)) * time.Second
}