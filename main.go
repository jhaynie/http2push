package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/karlseguin/ccache"
	"github.com/vulcand/oxy/forward"
	"github.com/vulcand/oxy/testutils"
	"golang.org/x/net/html"
)

var (
	Build     string
	BuildDate string
	Version   string

	port         int
	forwardPort  int
	forwardProto string
	cacheMaxAge  int
	docBase      string
	cacheMaxSize int64
	selfSigned   bool
	version      bool
	certFile     string
	keyFile      string

	cache *ccache.Cache
)

func init() {
	flag.BoolVar(&version, "version", false, "show version and exit")
	flag.StringVar(&docBase, "basedir", "/app/html", "base directory (defaults to '/app/html')")
	flag.IntVar(&port, "port", 9998, "port to listen on (defaults to 9998)")
	flag.IntVar(&forwardPort, "forwardPort", 9999, "port to forward non-static URL (defaults to 9999)")
	flag.StringVar(&forwardProto, "forwardProto", "http", "protocol to forward non-static URL (defaults to 'http')")
	flag.Int64Var(&cacheMaxSize, "maxsize", 500000, "max cache size in KB (defaults to 500,000)")
	flag.BoolVar(&selfSigned, "selfsigned", false, "allow self-signed TLS certificates (defaults to false)")
	flag.StringVar(&certFile, "cert", "/cert.crt", "location to the TLS certificate file (defaults to /cert.crt)")
	flag.StringVar(&keyFile, "key", "/cert.key", "location to the TLS certificate private key (defaults to /cert.key)")
	flag.IntVar(&cacheMaxAge, "maxage", 60, "cache max-age value in seconds (defaults to 60)")
	flag.Parse()
}

type CacheItem struct {
	buffer      []byte
	gzipbuf     []byte
	pushurls    []string
	parsed      bool
	contenttype string
	ishtml      bool
	hits        int64
	etag        string
}

func (item *CacheItem) Size() int64 {
	return int64(len(item.buffer))
}

func (item *CacheItem) String() string {
	return string(item.buffer)
}

func (item *CacheItem) AddURL(url string) {
	if item.pushurls == nil {
		item.pushurls = make([]string, 0)
	}
	item.pushurls = append(item.pushurls, url)
}

func (item *CacheItem) IsUnchanged(w http.ResponseWriter, req *http.Request) bool {
	if req.Header.Get("if-none-match") == item.etag {
		w.WriteHeader(304)
		fmt.Fprintf(w, "Not Modified")
		return true
	}
	return false
}

func gzipbuf(buf []byte) []byte {
	var b bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&b, 9)
	if gz != nil {
		_, err := gz.Write(buf)
		if err == nil {
			gz.Flush()
			gz.Close()
		}
	}
	return b.Bytes()
}

func NewCacheItem(buf []byte, fn string) *CacheItem {
	mt := mime.TypeByExtension(filepath.Ext(fn))
	ishtml := strings.Contains(mt, "text/html")
	hash := sha1.New()
	hash.Write(buf)
	etag := hex.EncodeToString(hash.Sum(nil))
	return &CacheItem{buf, gzipbuf(buf), nil, false, mt, ishtml, 0, etag}
}

func getenv(key string, defval string) string {
	v := os.Getenv(key)
	if v == "" {
		return defval
	}
	return v
}

type ShutdownHookFunc func()

// create a signal handler and a channel which we will notify
// upon receiving a SIGINT, SIGTERM or interrupt
func createShutdownHook(shutdown ShutdownHookFunc) chan bool {
	done := make(chan bool)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-c
		log.Infof("%v signal received, shutting down...", s)
		shutdown()
		done <- true
	}()
	return done
}

// walk through the HTML content and see if we can find any
// <link rel="push" href="...">
// elements and if so, pull them out so that we can HTTP/2 push
// them to clients that support it
func findPushLinks(reader io.Reader) ([]string, string, error) {
	doc, err := html.Parse(reader)
	if err != nil {
		return nil, "", err
	}
	urls := make([]string, 0)
	nodes := make([]*html.Node, 0)
	var visitor func(*html.Node)
	visitor = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "link" {
			var relFound, noPush bool
			var href string
			for _, a := range node.Attr {
				if a.Key == "rel" {
					if a.Val == "push" || a.Val == "preload" {
						relFound = true
					} else {
						break
					}
				} else if a.Key == "href" {
					href = a.Val
				} else if a.Key == "nopush" {
					noPush = true
				}
			}
			if relFound && noPush == false && href != "" {
				urls = append(urls, href)
				nodes = append(nodes, node)
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			visitor(c)
		}
	}
	visitor(doc)
	for _, node := range nodes {
		// remove it since it's only for server side use
		node.Parent.RemoveChild(node)
	}
	var content bytes.Buffer
	html.Render(&content, doc)
	return urls, content.String(), nil
}

// parseEncodings and parseCoding are from https://github.com/NYTimes/gziphandler/blob/master/gzip.go
// licensed as Apache-2 and relicensed the same

type codings map[string]float64

// The default qvalue to assign to an encoding if no explicit qvalue is set.
// This is actually kind of ambiguous in RFC 2616, so hopefully it's correct.
// The examples seem to indicate that it is.
const DEFAULT_QVALUE = 1.0

// parseEncodings attempts to parse a list of codings, per RFC 2616, as might
// appear in an Accept-Encoding header. It returns a map of content-codings to
// quality values, and an error containing the errors encountered. It's probably
// safe to ignore those, because silently ignoring errors is how the internet
// works.
//
// See: http://tools.ietf.org/html/rfc2616#section-14.3.
func parseEncodings(s string) (codings, error) {
	c := make(codings)
	var e []string

	for _, ss := range strings.Split(s, ",") {
		coding, qvalue, err := parseCoding(ss)

		if err != nil {
			e = append(e, err.Error())
		} else {
			c[coding] = qvalue
		}
	}

	// TODO (adammck): Use a proper multi-error struct, so the individual errors
	//                 can be extracted if anyone cares.
	if len(e) > 0 {
		return c, fmt.Errorf("errors while parsing encodings: %s", strings.Join(e, ", "))
	}

	return c, nil
}

// parseCoding parses a single conding (content-coding with an optional qvalue),
// as might appear in an Accept-Encoding header. It attempts to forgive minor
// formatting errors.
func parseCoding(s string) (coding string, qvalue float64, err error) {
	for n, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		qvalue = DEFAULT_QVALUE

		if n == 0 {
			coding = strings.ToLower(part)
		} else if strings.HasPrefix(part, "q=") {
			qvalue, err = strconv.ParseFloat(strings.TrimPrefix(part, "q="), 64)

			if qvalue < 0.0 {
				qvalue = 0.0
			} else if qvalue > 1.0 {
				qvalue = 1.0
			}
		}
	}

	if coding == "" {
		err = fmt.Errorf("empty content-coding")
	}

	return
}

// stream the cache item back to the client
func stream(w http.ResponseWriter, req *http.Request, item *CacheItem) {
	item.hits++
	w.Header().Set("Etag", fmt.Sprintf("W/\"%s\"", item.etag))
	w.Header().Set("Content-Type", item.contenttype)
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d, must-revalidate", cacheMaxAge))
	w.Header().Set("X-Cache-Hit", fmt.Sprintf("%d", item.hits))
	w.Header().Add("Vary", "Accept-Encoding")
	// check to see if we can send compressed data
	acceptedEncodings, _ := parseEncodings(req.Header.Get("Accept-Encoding"))
	if acceptedEncodings["gzip"] > 0.0 && len(item.gzipbuf) > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(item.gzipbuf)))
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(item.gzipbuf)
	} else {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", item.Size()))
		w.Write(item.buffer)
	}
}

// notfound will forward the request to the backend since we didn't handle it.
// this allows the back-end to server any 404 logic and control the presentation
func notfound(fwd *forward.Forwarder, w http.ResponseWriter, req *http.Request) {
	req.URL = testutils.ParseURI(fmt.Sprintf("%s://127.0.0.1:%d%s", forwardProto, forwardPort, req.URL))
	fwd.ServeHTTP(w, req)
}

// attempt to push any push urls (if found and if the HTTP client supports HTTP/2)
func push(pusher http.Pusher, w http.ResponseWriter, req *http.Request, item *CacheItem) {
	// check to see if the content target is an HTML doc
	if item.ishtml {
		// if not yet parsed, we will parse the HTML
		if item.parsed == false {
			// attempt to extract push links from the parsed AST
			links, body, err := findPushLinks(bytes.NewReader(item.buffer))
			if err == nil {
				// add each url found from the doc
				for _, link := range links {
					item.AddURL(link)
				}
				// since we remove the links, we need to set the updated body
				item.buffer = []byte(body)
				item.gzipbuf = gzipbuf(item.buffer)
			}
			item.parsed = true
		}
		// if we have push urls, go ahead and push them
		if item.pushurls != nil && len(item.pushurls) > 0 {
			for _, pushurl := range item.pushurls {
				headers := http.Header{}
				ext := filepath.Ext(pushurl)
				headers.Set("Content-Type", mime.TypeByExtension(ext))
				headers.Set("Cache-Control", fmt.Sprintf("public, max-age=%d, must-revalidate", cacheMaxAge))
				opts := &http.PushOptions{
					Method: "GET",
					Header: headers,
				}
				if err := pusher.Push(pushurl, opts); err != nil {
					log.Errorf("error pushing %s. %v", pushurl, err)
				}
			}
		}
	}
	// stream the regular content back to the client
	stream(w, req, item)
}

func main() {

	if version {
		fmt.Println(Version)
		os.Exit(0)
	}

	// create a cache to hold static files that we find
	cache = ccache.New(ccache.Configure().MaxSize(cacheMaxSize))

	// create a forward interface
	fwd, _ := forward.New()
	redirect := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// clean the incoming path
		fn := filepath.Clean(req.URL.Path)
		if fn == "" {
			fn = "/"
		}
		// try and find the static file relative to our base
		p := path.Join(docBase, fn)
		fi, err := os.Stat(p)
		if err != nil && os.IsNotExist(err) {
			notfound(fwd, w, req)
			return
		}
		if fi.IsDir() {
			p = path.Join(p, "index.html")
			if _, err = os.Stat(p); os.IsNotExist(err) {
				notfound(fwd, w, req)
				return
			}
		}
		// see if this incoming client request supports HTTP2 push
		pusher, ok := w.(http.Pusher)
		// try and get the path from the cache
		item := cache.Get(p)
		if item != nil {
			ci := item.Value().(*CacheItem)
			log.Infof("cache hit %s (%d)", req.URL, ci.hits)
			if ci.IsUnchanged(w, req) {
				return
			}
			if ok {
				push(pusher, w, req, ci)
			} else {
				stream(w, req, ci)
			}
			return
		}
		// cache miss, read the file in and create the cache entry
		buf, err := ioutil.ReadFile(p)
		if err != nil {
			notfound(fwd, w, req)
			return
		}
		// add it to the cache with a long expiration (let the size be the driver of cache)
		cacheitem := NewCacheItem(buf, p)
		cache.Set(p, cacheitem, 365*24*time.Hour)
		log.Infof("cache miss %s", req.URL)
		if ok {
			push(pusher, w, req, cacheitem)
		} else {
			stream(w, req, cacheitem)
		}
	})

	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: selfSigned,
	}
	srv := &http.Server{
		TLSConfig: cfg,
		Handler:   redirect,
		Addr:      fmt.Sprintf(":%d", port),
	}
	shutdown := func() {
		// be graceful about it
		srv.Shutdown(context.Background())
	}

	// in case anyone uses fatal
	log.RegisterExitHandler(shutdown)

	// create a shutdown hook handler
	done := createShutdownHook(shutdown)

	// start our webserver (in a separate go routine since it blocks)
	go func() {
		log.Infof("listening on %s, will proxy to :%d", srv.Addr, forwardPort)
		err := srv.ListenAndServeTLS(certFile, keyFile)
		if err != nil && err != http.ErrServerClosed {
			log.Errorf("error HTTP listen %v", err)
			os.Exit(1)
		}
	}()

	//TEMP remove this once we have things all tidied up and working
	go func() {
		handler := func(w http.ResponseWriter, r *http.Request) {
			// fmt.Println("INCOMING", r.URL)
			fmt.Fprintf(w, "Hi there, I love %s!", r.URL.Path[1:])
		}
		http.HandleFunc("/", handler)
		err := http.ListenAndServe(fmt.Sprintf(":%d", forwardPort), nil)
		if err != nil {
			fmt.Println(err)
		}
	}()

	// wait for our signal handler to notify the channel that we're done
	<-done
}
