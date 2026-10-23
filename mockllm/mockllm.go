// Package mockllm is a deterministic stand-in for an LLM: a pure function
// of (prompt, seed, params). It parses the structured fields out of the
// prompt and composes an answer from them, with a seeded phrasing choice.
// Because it is deterministic, a replayed call with the same prompt bytes
// and seed must produce byte-identical output — which is exactly the
// property traceweave's replay engine verifies.
//
// The mock also has one documented quirk, standing in for a real model
// failure mode: for seeds with seed%7==3 it misreads the PRICE line,
// answering with the STOCK value as the price. The quirk is a function of
// the recorded seed only, so replay reproduces it faithfully, and replay
// with a different seed removes it.
package mockllm

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// Params are the recorded call parameters.
type Params struct {
	Model       string
	Temperature float64
	Seed        int64
}

// QuirkSeed reports whether the seed triggers the mock's misread quirk.
func QuirkSeed(seed int64) bool {
	return seed%7 == 3
}

var closers = []string{
	"Let me know if you need anything else.",
	"Happy to help with anything else.",
	"Just say the word if you need more detail.",
	"Anything else I can check for you?",
}

// field extracts the value of a "KEY: value" line from the prompt.
func field(prompt, key string) string {
	prefix := key + ":"
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

// Generate produces the model's answer. Pure and deterministic.
func Generate(prompt string, p Params) string {
	product := field(prompt, "PRODUCT")
	price := field(prompt, "PRICE")
	status := field(prompt, "STATUS")
	if QuirkSeed(p.Seed) {
		// The quirk: the model reads the wrong line as the price.
		price = field(prompt, "STOCK")
	}
	h := fnv.New64a()
	h.Write([]byte(prompt))
	h.Write([]byte(fmt.Sprintf("|%d|%s|%v", p.Seed, p.Model, p.Temperature)))
	closer := closers[int(h.Sum64()%uint64(len(closers)))]
	return fmt.Sprintf("The %s costs $%s and is currently %s. %s", product, price, status, closer)
}
