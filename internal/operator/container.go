// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import "fmt"

// letterValues are ISO 6346's equivalent values for A-Z: counting up from
// 10, skipping multiples of 11.
var letterValues = func() map[byte]int {
	m := map[byte]int{}
	v := 10
	for c := byte('A'); c <= 'Z'; c++ {
		if v%11 == 0 {
			v++
		}
		m[c] = v
		v++
	}
	return m
}()

// containerNumber is an ISO 6346 container number: the 4-letter owner code
// and category (e.g. "OCBU"), a 6-digit serial, and the check digit.
func containerNumber(prefix string, serial int) string {
	base := fmt.Sprintf("%s%06d", prefix, serial%1_000_000)
	return base + fmt.Sprint(checkDigit(base))
}

// checkDigit computes ISO 6346's check digit over the first 10 characters:
// each character's value times 2^position, summed, mod 11, mod 10.
func checkDigit(s string) int {
	sum := 0
	for i := 0; i < 10; i++ {
		c := s[i]
		v, ok := letterValues[c]
		if !ok {
			v = int(c - '0')
		}
		sum += v << i
	}
	return sum % 11 % 10
}
