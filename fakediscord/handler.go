// Copyright 2021 The zombiezen Go Client for Discord Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//		 https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package fakediscord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

type apiRequest struct {
	host     string
	pathVars map[string]string
	header   http.Header
	body     json.RawMessage
}

type apiResponse struct {
	statusCode int
	body       json.RawMessage
}

func newResponse(statusCode int, value interface{}) (*apiResponse, error) {
	resp := &apiResponse{statusCode: statusCode}
	var err error
	resp.body, err = json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (resp *apiResponse) writeTo(w http.ResponseWriter) {
	data, err := json.Marshal(resp.body)
	if err != nil {
		writeErrorResponse(w, fmt.Errorf("could not marshal response: %w", err))
		return
	}
	w.Header().Set(contentTypeHeaderName, "application/json; charset=utf-8")
	w.Header().Set(contentLengthHeaderName, strconv.Itoa(len(data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if resp.statusCode != 0 {
		w.WriteHeader(resp.statusCode)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	w.Write(resp.body)
}

type apiError struct {
	err            error
	discordCode    int
	httpStatusCode int
}

func (e *apiError) Error() string {
	return e.err.Error()
}

func (e *apiError) Unwrap() error {
	return e.err
}

type apiHandlerFunc func(ctx context.Context, r *apiRequest) (*apiResponse, error)

func (h apiHandlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	areq := &apiRequest{
		host:     r.Host,
		pathVars: mux.Vars(r),
		header:   r.Header,
	}
	if r.ContentLength != 0 { // includes unknown length
		if err := readJSONRequest(r, &areq.body); err != nil {
			writeErrorResponse(w, &apiError{
				httpStatusCode: http.StatusBadRequest,
				err:            fmt.Errorf("failed to read request body: %w", err),
			})
			return
		}
	}
	aresp, err := h(r.Context(), areq)
	if err != nil {
		writeErrorResponse(w, err)
		return
	}
	aresp.writeTo(w)
}

func readJSONRequest(r *http.Request, dst interface{}) error {
	ctHeader := r.Header.Get(contentTypeHeaderName)
	if ct, _, err := mime.ParseMediaType(ctHeader); err != nil || ct != "application/json" {
		return fmt.Errorf(
			"read json request: %s = %s (want application/json)",
			contentTypeHeaderName, ctHeader,
		)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("read json request: %v", err)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("read json request: %v", err)
	}
	return nil
}

func writeErrorResponse(w http.ResponseWriter, err error) {
	discordErrorCode := 0
	httpStatusCode := http.StatusInternalServerError
	if apiErr := (*apiError)(nil); errors.As(err, &apiErr) {
		discordErrorCode = apiErr.discordCode
		if apiErr.httpStatusCode != 0 {
			httpStatusCode = apiErr.httpStatusCode
		}
	}
	data, err := json.Marshal(map[string]interface{}{
		"code":    discordErrorCode,
		"message": err.Error(),
	})
	if err != nil {
		data = []byte(`{"code": 0, "message": "<failed to marshal error>"}`)
	}
	w.Header().Set(contentTypeHeaderName, "application/json; charset=utf-8")
	w.Header().Set(contentLengthHeaderName, strconv.Itoa(len(data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(httpStatusCode)
	w.Write(data)
}

var errUnauthorized error = &apiError{
	discordCode:    40001,
	err:            errors.New("invalid Authorization header"),
	httpStatusCode: http.StatusUnauthorized,
}
