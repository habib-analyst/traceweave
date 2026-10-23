package mockllm

import (
	"strings"
	"testing"
)

const prompt = `You are a support assistant. Answer using only the facts below.
USER: Amara Chen
TIER: gold
PRODUCT: Aurora Headphones
PRICE: 129
STATUS: in stock
STOCK: 34
QUESTION: What is the price and availability of Aurora Headphones?`

func TestDeterministic(t *testing.T) {
	p := Params{Model: "mock-1", Temperature: 0, Seed: 42}
	a, b := Generate(prompt, p), Generate(prompt, p)
	if a != b {
		t.Fatalf("not deterministic:\n%s\n%s", a, b)
	}
	if !strings.Contains(a, "$129") || !strings.Contains(a, "in stock") {
		t.Fatalf("answer does not carry prompt facts: %s", a)
	}
}

func TestQuirkSeedMisreadsPrice(t *testing.T) {
	var quirk int64 = 3
	if !QuirkSeed(quirk) {
		t.Fatal("seed 3 should be a quirk seed")
	}
	out := Generate(prompt, Params{Model: "mock-1", Seed: quirk})
	if !strings.Contains(out, "$34") {
		t.Fatalf("quirk output should state the STOCK value as price: %s", out)
	}
	// A neighbouring seed answers correctly.
	out = Generate(prompt, Params{Model: "mock-1", Seed: 4})
	if !strings.Contains(out, "$129") {
		t.Fatalf("non-quirk output wrong: %s", out)
	}
}
