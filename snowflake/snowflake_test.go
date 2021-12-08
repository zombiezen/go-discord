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

package snowflake

import (
	"testing"
	"time"
)

var discordEpoch = time.UnixMilli(1420070400000)

func TestID(t *testing.T) {
	tests := []struct {
		id        ID
		timestamp uint64
		instance  uint16
		sequence  uint16
	}{
		{
			id:        0,
			timestamp: 0,
			instance:  0,
			sequence:  0,
		},
		{
			// Canonical example from
			// https://discord.com/developers/docs/reference#convert-snowflake-to-datetime
			id:        0b000000100111000100000110010110101100000100_00001_00000_000000000111,
			timestamp: 41944705796,
			instance:  0b0000100000,
			sequence:  0b000000000111,
		},
	}

	t.Run("New", func(t *testing.T) {
		for _, test := range tests {
			got := New(test.timestamp, test.instance, test.sequence)
			if got != test.id {
				t.Errorf(
					"New(%#041b, %#010b, %#012b) =\n%#064b; want\n%#064b",
					test.timestamp, test.instance, test.sequence, got, test.id,
				)
			}
		}
	})

	t.Run("Timestamp", func(t *testing.T) {
		for _, test := range tests {
			if got, want := test.id.Timestamp(), test.timestamp; got != want {
				t.Errorf(
					"snowflake.ID(%#064b).Timestamp() = %#041b (%d); want %#041b (%d)",
					test.id, got, got, want, want,
				)
			}
		}
	})

	t.Run("Instance", func(t *testing.T) {
		for _, test := range tests {
			if got, want := test.id.Instance(), test.instance; got != want {
				t.Errorf(
					"snowflake.ID(%#064b).Instance() = %#010b (%d); want %#010b (%d)",
					test.id, got, got, want, want,
				)
			}
		}
	})

	t.Run("Sequence", func(t *testing.T) {
		for _, test := range tests {
			if got, want := test.id.Sequence(), test.sequence; got != want {
				t.Errorf(
					"snowflake.ID(%#064b).Sequence() = %#012b (%d); want %#012b (%d)",
					test.id, got, got, want, want,
				)
			}
		}
	})
}

func TestGenerator(t *testing.T) {
	const instance = 0x2ab
	gen := NewGenerator(discordEpoch, instance)
	const n = 100
	ids := make(map[ID]struct{}, n)
	for i := 0; i < n; i++ {
		got := gen.Generate()
		if gotInstance := got.Instance(); gotInstance != instance {
			t.Errorf(
				"ID[%d] = %#064b (instance = %#x); want instance = %#x",
				i, got, gotInstance, instance,
			)
		}
		ids[got] = struct{}{}
	}
	if len(ids) != n {
		t.Errorf("Generator produced %d duplicates", n-len(ids))
	}
}

func BenchmarkGenerator(b *testing.B) {
	g := NewGenerator(discordEpoch, 42)
	for i := 0; i < b.N; i++ {
		g.Generate()
	}
}
