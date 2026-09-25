package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
)

// Paging bounds of every paged list (design §7: page >= 1, pageSize 1-500, default 50).
const (
	defaultPageSize = jobqueue.DefaultPageSize
	maxPageSize     = jobqueue.MaxPageSize
)

// pathID reads the {id} URL parameter: a positive integer.
func pathID(r *http.Request) (int64, error) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, errorf(http.StatusBadRequest, "invalid id %q: it must be a positive integer", raw)
	}
	return id, nil
}

// paging reads the page and pageSize query parameters: page >= 1 (default 1), pageSize 1-500
// (default 50).
func paging(q url.Values) (page, size int, err error) {
	page, err = intParam(q, "page", 1, 1, 1<<30)
	if err != nil {
		return 0, 0, err
	}
	size, err = intParam(q, "pageSize", defaultPageSize, 1, maxPageSize)
	if err != nil {
		return 0, 0, err
	}
	return page, size, nil
}

// intParam reads an optional integer query parameter in [lo, hi].
func intParam(q url.Values, name string, def, lo, hi int) (int, error) {
	raw := strings.TrimSpace(q.Get(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		return 0, errorf(http.StatusBadRequest, "%s must be a whole number from %d to %d", name, lo, hi)
	}
	return n, nil
}

// int64Param reads an optional non-negative integer query parameter (0 when absent).
func int64Param(q url.Values, name string) (int64, error) {
	raw := strings.TrimSpace(q.Get(name))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errorf(http.StatusBadRequest, "%s must be a whole number of at least 0", name)
	}
	return n, nil
}

// decodeBody is decodeJSON with the error classified as a 400.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	if err := decodeJSON(w, r, v); err != nil {
		return &httpError{status: http.StatusBadRequest, msg: err.Error()}
	}
	return nil
}

// decodeOptionalBody is decodeBody for endpoints whose body may be left out: an empty body keeps
// v's zero value; anything else must be a single JSON object without unknown fields.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, v any) error {
	br := bufio.NewReader(http.MaxBytesReader(w, r.Body, maxBody))
	if _, err := br.Peek(1); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errorf(http.StatusBadRequest, "request body is too large")
		}
		return errorf(http.StatusBadRequest, "cannot read the request body: %v", err)
	}
	dec := json.NewDecoder(br)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errorf(http.StatusBadRequest, "request body is too large")
		}
		return errorf(http.StatusBadRequest, "invalid JSON body: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errorf(http.StatusBadRequest, "invalid JSON body: expected a single object")
	}
	return nil
}

// writeAccepted answers a job-starting request: 202 with the job and its URL.
func writeAccepted(w http.ResponseWriter, jobID int64, job any) {
	w.Header().Set("Location", fmt.Sprintf("/api/v1/jobs/%d", jobID))
	writeJSON(w, http.StatusAccepted, job)
}

// jsonMarshal encodes v, wrapping the (unexpected) error with context.
func jsonMarshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode %T: %w", v, err)
	}
	return b, nil
}
