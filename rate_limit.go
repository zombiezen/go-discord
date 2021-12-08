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

package discord

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Discord rate limit headers.
// https://discord.com/developers/docs/topics/rate-limits
const (
	rateLimitBucketHeader     = "X-Ratelimit-Bucket"
	rateLimitRemainingHeader  = "X-Ratelimit-Remaining"
	rateLimitResetAfterHeader = "X-Ratelimit-Reset-After"
	rateLimitGlobalHeader     = "X-Ratelimit-Global"
)

type rateLimit struct {
	remaining int
	reset     time.Time
}

type rateLimitKey struct {
	bucket     string
	majorParam string
}

type rateLimitArg struct {
	route      string
	majorParam string
}

func (arg rateLimitArg) toKey(bucket string) rateLimitKey {
	return rateLimitKey{bucket: bucket, majorParam: arg.majorParam}
}

func (req *apiRequest) rateLimitRoute() string {
	return req.method + " " + req.route
}

func (req *apiRequest) rateLimitMajorParam() string {
	switch {
	case req.pathVars["channel.id"] != "":
		return req.pathVars["channel.id"]
	case req.pathVars["interaction.id"] != "":
		return req.pathVars["interaction.id"] + "/" + req.pathVars["interaction.token"]
	case req.pathVars["webhook.id"] != "":
		return req.pathVars["webhook.id"] + "/" + req.pathVars["webhook.token"]
	default:
		return ""
	}
}

func (c *Client) waitForRateLimit(ctx context.Context, req *apiRequest) error {
	span := trace.SpanFromContext(ctx)
	route := req.rateLimitRoute()
	for {
		now := time.Now()
		c.mu.Lock()
		nextReset := c.globalRateLimitReset
		bucket := c.rateLimitBuckets[route]
		if bucket != "" {
			key := rateLimitKey{
				bucket:     bucket,
				majorParam: req.rateLimitMajorParam(),
			}
			if limit := c.rateLimits[key]; limit != nil {
				if limit.remaining <= 0 && limit.reset.After(nextReset) {
					nextReset = limit.reset
				}
				if now.Before(nextReset) && limit.remaining > 0 {
					limit.remaining--
				}
			}
		}
		c.mu.Unlock()

		if now.After(nextReset) {
			return nil
		}
		waitTime := nextReset.Sub(now)
		span.AddEvent(fmt.Sprintf("Throttling request on %s for %v (bucket=%q)", route, waitTime, bucket))
		t := time.NewTimer(waitTime)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("waiting for rate limit: %w", ctx.Err())
		}
	}
}
