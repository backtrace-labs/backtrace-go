// Package fakebacktrace is a local stand-in for the Backtrace submission
// endpoint, used by the SDK's examples and tests.
package fakebacktrace

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"

	bt "github.com/backtrace-labs/backtrace-go"
)

type Report struct {
	Attributes  map[string]any `json:"attributes"`
	Annotations map[string]any `json:"annotations"`
	Classifiers []string       `json:"classifiers"`

	Breadcrumbs []bt.Breadcrumb `json:"-"`
}

func (r Report) BreadcrumbMessages() []string {
	messages := []string{}
	for _, b := range r.Breadcrumbs {
		messages = append(messages, b.Message)
	}
	return messages
}

type Server struct {
	*httptest.Server

	Endpoint string

	mu      sync.Mutex
	reports []Report
}

func Start() *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.receive))
	s.Endpoint = s.URL + "/example-universe/example-token/json"
	return s
}

func (s *Server) Reports() []Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Report(nil), s.reports...)
}

func (s *Server) receive(w http.ResponseWriter, r *http.Request) {
	report, err := Decode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.reports = append(s.reports, report)
	s.mu.Unlock()
	_, _ = io.WriteString(w, `{"response":"ok"}`)
}

func Decode(r *http.Request) (Report, error) {
	var report Report
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return report, err
		}
		body = zr
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return report, err
	}
	if params["boundary"] == "" {
		return report, json.NewDecoder(body).Decode(&report)
	}
	parts := multipart.NewReader(body, params["boundary"])
	for {
		part, err := parts.NextPart()
		if errors.Is(err, io.EOF) {
			return report, nil
		}
		if err != nil {
			return report, err
		}
		switch part.FormName() {
		case "upload_file":
			err = json.NewDecoder(part).Decode(&report)
		case "attachment_bt-breadcrumbs-0":
			err = json.NewDecoder(part).Decode(&report.Breadcrumbs)
		}
		if err != nil {
			return report, err
		}
	}
}
