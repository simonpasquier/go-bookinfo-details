// Copyright 2019 Simon Pasquier
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mwitkow/go-conntrack"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/api/books/v1"
)

var (
	help                   bool
	listen                 string
	timeout, delay, expiry time.Duration
	errorRatio             float64
	gen                    *rand.Rand
	store                  responseCache

	incomingHeaders = []string{
		"x-request-id",
		"x-b3-traceid",
		"x-b3-spanid",
		"x-b3-parentspanid",
		"x-b3-sampled",
		"x-b3-flags",
		"x-ot-span-context",
	}
)

var (
	incomingDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "details_incoming_requests_duration_seconds",
			Help:    "Histogram of incoming request latencies.",
			Buckets: []float64{.1, .5, 1, 1.5, 2, 5},
		},
		[]string{"code"},
	)
	outgoingDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "details_outgoing_requests_duration_seconds",
			Help:    "Histogram of request latencies to the downstream API.",
			Buckets: []float64{.1, .5, 1, 1.5, 2, 5},
		},
		[]string{"code"},
	)
	inflightRequests = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "details_incoming_requests_in_flight",
		Help: "Number of in-flight requests to the details service.",
	})
	cacheSize = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "details_cache_size",
			Help: "Number of items in the in-memory cache",
		},
		func() float64 {
			return float64(store.length())
		},
	)
)

func init() {
	flag.BoolVar(&help, "help", false, "Help message")
	flag.StringVar(&listen, "listen-address", ":8080", "Listen address")
	flag.DurationVar(&timeout, "timeout", 5*time.Second, "Maximum duration to wait for downstream API")
	flag.DurationVar(&delay, "delay", 0*time.Second, "Artifical delay to wait after receiving the response from the downstream API")
	flag.DurationVar(&expiry, "cache-expiry", 0*time.Second, "How long to keep objects in the cache")
	flag.Float64Var(&errorRatio, "error", 0.0, "Ratio of injected error responses")
	if errorRatio < 0.0 {
		errorRatio = 0.0
	}
	if errorRatio > 1.0 {
		errorRatio = 1.0
	}
	gen = rand.New(rand.NewSource(time.Now().UnixNano()))

	prometheus.MustRegister(incomingDuration, outgoingDuration, inflightRequests)
	for _, c := range []int{http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		incomingDuration.WithLabelValues(fmt.Sprintf("%d", c))
	}
}

type errorResponse struct {
	Message string
}

type statusResponse struct {
	Message string
}

type detailsResponse struct {
	ID        int    `json:"id"`
	Author    string `json:"author"`
	Year      string `json:"year"`
	Type      string `json:"type"`
	Pages     int64  `json:"pages"`
	Publisher string `json:"publisher"`
	Language  string `json:"language"`
	Isbn10    string `json:"ISBN-10"`
	Isbn13    string `json:"ISBN-13"`
}

type responseCache interface {
	get(int) *detailsResponse
	set(*detailsResponse)
	length() int
}

type noopCache struct{}

func (c *noopCache) get(int) *detailsResponse { return nil }
func (c *noopCache) set(*detailsResponse)     {}
func (c *noopCache) length() int              { return 0 }

type cache struct {
	expiry  time.Duration
	mtx     sync.Mutex
	entries map[int]*cacheEntry
}

type cacheEntry struct {
	response *detailsResponse
	ttl      time.Time
}

func newCache(expiry time.Duration) *cache {
	c := &cache{
		mtx:     sync.Mutex{},
		entries: make(map[int]*cacheEntry),
		expiry:  expiry,
	}
	go func() {
		for {
			select {
			case <-time.After(1 * time.Second):
				c.mtx.Lock()
				now := time.Now()
				for k := range c.entries {
					if c.entries[k].ttl.Before(now) {
						delete(c.entries, k)
					}
				}
				c.mtx.Unlock()
			}
		}
	}()
	return c
}

func (c *cache) get(id int) *detailsResponse {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	e, ok := c.entries[id]
	if !ok {
		return nil
	}
	if time.Now().After(e.ttl) {
		return nil
	}
	return e.response
}

func (c *cache) set(d *detailsResponse) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	c.entries[d.ID] = &cacheEntry{
		response: d,
		ttl:      time.Now().Add(c.expiry),
	}
}

func (c *cache) length() int {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return len(c.entries)
}

func writeResponseError(w http.ResponseWriter, code int, e error) {
	b, err := json.Marshal(&errorResponse{Message: fmt.Sprintf("%s", e)})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(code)
	w.Write(b)
}

func writeResponseOK(w http.ResponseWriter, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

func getISBN(identifiers []*books.VolumeVolumeInfoIndustryIdentifiers, typ string) string {
	for _, identifier := range identifiers {
		if identifier.Type == typ {
			return identifier.Identifier
		}
	}
	return ""
}

func details(w http.ResponseWriter, r *http.Request) {
	inflightRequests.Inc()
	defer inflightRequests.Dec()

	var (
		err  error
		code = http.StatusOK
	)
	defer func() {
		if err != nil {
			log.Printf("/details/ error: %q", err)
			writeResponseError(w, code, err)
		}
	}()

	// Inject some errors if required.
	if errorRatio > 0.0 && gen.Float64() < errorRatio {
		err = fmt.Errorf("random error")
		code = http.StatusServiceUnavailable
		return
	}

	isbn := strings.TrimPrefix(r.URL.Path, "/details/")
	if isbn == "0" {
		// The productpage application always send 0 as the ISBN so
		// hard-code here with one of the ISBN for "The comedy of errors".
		isbn = "0486424618"
	}
	id, err := strconv.Atoi(isbn)
	if err != nil {
		code = http.StatusBadRequest
		return
	}
	if v := store.get(id); v != nil {
		writeResponseOK(w, v)
		return
	}

	svc, err := books.New(
		&http.Client{
			Transport: promhttp.InstrumentRoundTripperDuration(outgoingDuration, &http.Transport{
				IdleConnTimeout: 1 * time.Minute,
				DialContext: conntrack.NewDialContextFunc(
					conntrack.DialWithTracing(),
					conntrack.DialWithName("google-api"),
				),
			}),
		},
	)
	if err != nil {
		//TODO: implement fallback response
		code = http.StatusInternalServerError
		return
	}

	volService := books.NewVolumesService(svc)
	volCall := volService.List(fmt.Sprintf("isbn:%s", isbn))

	ctx := r.Context()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	volCall = volCall.Context(ctx)

	// Add tracing headers.
	header := volCall.Header()
	for _, h := range incomingHeaders {
		header.Add(h, r.Header.Get(h))
	}

	// Send the request to the downstream API.
	vols, err := volCall.Do()
	if err != nil {
		code = http.StatusInternalServerError
		if ctx.Err() != nil {
			code = http.StatusServiceUnavailable
		}
		return
	}
	if len(vols.Items) == 0 {
		err = fmt.Errorf("ISBN %s not found", isbn)
		code = http.StatusNotFound
		return
	}

	vol := vols.Items[0].VolumeInfo

	book := &detailsResponse{
		ID:        id,
		Author:    vol.Authors[0],
		Year:      vol.PublishedDate,
		Type:      vol.PrintType,
		Pages:     vol.PageCount,
		Publisher: vol.Publisher,
		Language:  vol.Language,
		Isbn10:    getISBN(vol.IndustryIdentifiers, "ISBN_10"),
		Isbn13:    getISBN(vol.IndustryIdentifiers, "ISBN_13"),
	}
	if vol.Language == "en" {
		book.Language = "English"
	}
	if vol.PrintType == "BOOK" {
		book.Type = "paperback"
	}
	store.set(book)

	<-time.After(delay)
	writeResponseOK(w, book)
}

func main() {
	flag.Parse()
	if help {
		fmt.Fprintln(os.Stderr, "Bookinfo details service")
		flag.PrintDefaults()
		os.Exit(0)
	}

	if expiry > time.Duration(0) {
		log.Printf("Using cache expiry (ttl=%v)", expiry)
		store = newCache(expiry)
	} else {
		store = &noopCache{}
	}

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeResponseOK(w, &statusResponse{Message: "OK"})
	})

	http.HandleFunc("/metrics", promhttp.Handler().ServeHTTP)
	http.Handle("/details/", promhttp.InstrumentHandlerInFlight(
		inflightRequests,
		promhttp.InstrumentHandlerDuration(incomingDuration,
			http.HandlerFunc(details),
		),
	))

	log.Println("Listening on", listen)
	log.Fatal(http.ListenAndServe(listen, nil))
}
