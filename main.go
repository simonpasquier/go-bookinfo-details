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
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mwitkow/go-conntrack"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/api/books/v1"
)

var (
	help           bool
	listen         string
	timeout, delay time.Duration

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
	requestsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "incoming_requests_total",
		Help: "Total number of requests to the details service",
	})
	failedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "incoming_requests_failed_total",
		Help: "Total number of requests to the details service that have failed",
	})
	duration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "outgoing_requests_duration",
			Help:    "Histogram of request latencies to the downstream API.",
			Buckets: []float64{.1, .5, 1, 1.5, 2, 5},
		},
		[]string{},
	)
	inflightRequests = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "incoming_requests_in_flight",
		Help: "Number of in-flight requests to the details service.",
	})
)

func init() {
	flag.BoolVar(&help, "help", false, "Help message")
	flag.StringVar(&listen, "listen-address", ":8080", "Listen address")
	flag.DurationVar(&timeout, "timeout", 5*time.Second, "Maximum duration to wait for downstream API")
	flag.DurationVar(&delay, "delay", 0*time.Second, "Artifical delay to wait after receiving the response from the downstream API")

	prometheus.MustRegister(requestsTotal, failedTotal, duration, inflightRequests)
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

func writeResponseBadRequest(w http.ResponseWriter, e error) {
	b, err := json.Marshal(&errorResponse{Message: fmt.Sprintf("%s", e)})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
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

func main() {
	flag.Parse()
	if help {
		fmt.Fprintln(os.Stderr, "Bookinfo details service")
		flag.PrintDefaults()
		os.Exit(0)
	}

	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeResponseOK(w, &statusResponse{Message: "OK"})
	})

	http.HandleFunc("/metrics", promhttp.Handler().ServeHTTP)

	http.HandleFunc("/details/", func(w http.ResponseWriter, r *http.Request) {
		requestsTotal.Inc()
		inflightRequests.Inc()
		defer inflightRequests.Dec()

		var err error
		defer func() {
			if err != nil {
				failedTotal.Inc()
				log.Printf("/details/ error: %q", err)
				writeResponseBadRequest(w, err)
			}
		}()
		isbn := strings.TrimPrefix(r.URL.Path, "/details/")
		if isbn == "0" {
			// The productpage application always send 0 as the ISBN so
			// hard-code here with one of the ISBN for "The comedy of errors".
			isbn = "0486424618"
		}
		i, err := strconv.Atoi(isbn)
		if err != nil {
			return
		}

		svc, err := books.New(
			&http.Client{
				Transport: promhttp.InstrumentRoundTripperDuration(duration, &http.Transport{
					IdleConnTimeout: 1 * time.Minute,
					DialContext: conntrack.NewDialContextFunc(
						conntrack.DialWithTracing(),
						conntrack.DialWithName("google-api"),
					),
				}),
			},
		)
		if err != nil {
			return
		}

		volService := books.NewVolumesService(svc)
		volCall := volService.List(fmt.Sprintf("isbn:%s", isbn))

		ctx := r.Context()
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		volCall = volCall.Context(ctx)

		// Add tracing headers
		header := volCall.Header()
		for _, h := range incomingHeaders {
			header.Add(h, r.Header.Get(h))
		}

		// Do request to downstream API.
		vols, err := volCall.Do()
		if err != nil {
			return
		}
		if len(vols.Items) == 0 {
			err = fmt.Errorf("found 0 items for ISBN %d", i)
			return
		}

		vol := vols.Items[0].VolumeInfo

		book := &detailsResponse{
			ID:        i,
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

		time.Sleep(delay)

		writeResponseOK(w, book)
	})

	log.Println("Listening on", listen)
	log.Fatal(http.ListenAndServe(listen, nil))
}
