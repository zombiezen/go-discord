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

// Package snowflake provides utilities for operating on Snowflake IDs.
// https://en.wikipedia.org/wiki/Snowflake_ID
package snowflake

import "time"

const (
	timestampBits = 41
	instanceBits  = 10
	sequenceBits  = 12
)

// ID is a Snowflake ID.
// It is 63 bits:
// the most significant bit should never be set
// (i.e. valid Snowflake IDs are non-negative).
type ID int64

// New builds a Snowflake ID from the component parts.
// Only the least significant 41, 10, and 12 bits are used
// from the timestamp, instance, and sequence arguments, respectively.
func New(timestamp uint64, instance, sequence uint16) ID {
	return ID((timestamp & (1<<timestampBits - 1) << (instanceBits + sequenceBits)) |
		(uint64(instance) & (1<<instanceBits - 1) << sequenceBits) |
		(uint64(sequence) & (1<<sequenceBits - 1)))
}

// Timestamp returns the 41-bit timestamp value of the ID.
// This is conventionally a number of milliseconds since a known epoch value.
func (id ID) Timestamp() uint64 {
	return uint64(id>>(instanceBits+sequenceBits)) & (1<<timestampBits - 1)
}

// Instance returns the 10-bit instance ID.
func (id ID) Instance() uint16 {
	return uint16(id>>sequenceBits) & (1<<instanceBits - 1)
}

// Sequence returns the 12-bit sequence number.
func (id ID) Sequence() uint16 {
	return uint16(id) & (1<<sequenceBits - 1)
}

// A Generator generates unique IDs.
// The zero value can be initialized with Init.
type Generator struct {
	epoch    uint64
	instance uint16
	seq      uint16
}

// NewGenerator returns a new generator
// that uses the given epoch and instance ID.
func NewGenerator(epoch time.Time, instance uint16) *Generator {
	gen := new(Generator)
	gen.Init(epoch, instance)
	return gen
}

// Init sets the generator's epoch and instance ID,
// and resets the generator's sequence number to zero.
func (gen *Generator) Init(epoch time.Time, instance uint16) {
	gen.epoch = uint64(unixMilli(epoch))
	gen.instance = instance
	gen.seq = 0
}

// Generate returns a unique ID.
// Generate must not be called concurrently on the same Generator.
func (gen *Generator) Generate() ID {
	seq := gen.seq
	gen.seq = (gen.seq + 1) & (1<<sequenceBits - 1)
	return New(
		uint64(unixMilli(time.Now()))-gen.epoch,
		gen.instance,
		seq,
	)
}
