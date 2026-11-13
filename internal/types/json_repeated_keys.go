// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// RefuseRepeatedJSONKeys refuses a JSON value with an object that has a key written twice, at any
// depth, and names that object by its path from root. Decoding keeps only the last of a repeated
// key, so `"denied_domains":["x"],"denied_domains":[]` would read as the empty list. data is one
// value a decoder has already read, so its syntax and its depth are already checked.
func RefuseRepeatedJSONKeys(data []byte, root string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // a number is passed over, never converted: its range is not this check's to refuse
	return refuseRepeatedKeys(dec, &[]string{root})
}

// refuseRepeatedKeys reads one JSON value from dec and refuses an object that has a key twice.
// path holds the steps down to the value being read and is joined only for the refusal: a joined
// path for every value would cost the square of the nesting depth in memory.
func refuseRepeatedKeys(dec *json.Decoder, path *[]string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for n := 0; dec.More(); n++ {
		step := "[" + strconv.Itoa(n) + "]"
		if delim == '{' {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			if seen[key] {
				return fmt.Errorf("%s has the key %q twice", strings.Join(*path, ""), key)
			}
			seen[key] = true
			step = "." + key
		}
		*path = append(*path, step)
		if err := refuseRepeatedKeys(dec, path); err != nil {
			return err
		}
		*path = (*path)[:len(*path)-1]
	}
	_, err = dec.Token()
	return err
}
