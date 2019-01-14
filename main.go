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
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/api/books/v1"
)

var (
	help         bool
	listen, file string

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

func init() {
	flag.BoolVar(&help, "help", false, "Help message")
	flag.StringVar(&listen, "listen-address", ":8080", "Listen address")
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
		var err error
		defer func() {
			if err != nil {
				writeResponseBadRequest(w, err)
			}
		}()
		isbn := strings.TrimPrefix(r.URL.Path, "/details/")
		i, err := strconv.Atoi(isbn)
		if err != nil {
			return
		}

		svc, err := books.New(
			&http.Client{
				Transport: &http.Transport{
					IdleConnTimeout: 5 * time.Second,
					DialContext: conntrack.NewDialContextFunc(
						conntrack.DialWithTracing(),
						conntrack.DialWithName("google-api"),
					),
				},
			},
		)
		//svc, err := books.New(c)
		if err != nil {
			return
		}

		volService := books.NewVolumesService(svc)
		volCall := volService.List(fmt.Sprintf("isbn:%s", isbn))

		ctx := r.Context()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		volCall = volCall.Context(ctx)

		// Add tracing headers
		header := volCall.Header()
		for _, h := range []string{} {
			header.Add(h, r.Header.Get(h))
		}

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
		writeResponseOK(w, book)
	})

	log.Println("Listening on", listen)
	log.Fatal(http.ListenAndServe(listen, nil))
}
