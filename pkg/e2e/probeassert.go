package e2e

import (
	"fmt"
	"regexp"
	"strings"
)

// Probe-answer evaluation for the clean-context isolation self-tests.
// Deliberately NOT e2e-tagged: the logic is pure and unit-tested in the
// regular suite, while the probes that call it stay behind the e2e tag.

// hardLeakTells are strings that must NEVER appear in a clean-context probe
// answer — each one is a specific host-config artifact from this machine
// class of leak (MCP servers, memory tooling, personas, house tooling).
var hardLeakTells = []string{
	"wp-mcp-router",
	"tessera",
	"cloudways",
	"AutoMem",
	"automem",
	"Apex",
	"PERSONALITY",
	"wp-test",
	"terse-build",
}

var probeListItemRe = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)])\s+(.*)$`)

// evaluateProbeAnswer checks a probe answer for contamination. It returns a
// list of violations; empty means clean.
//
// Rules:
//  1. A hard leak tell anywhere in the answer is a violation.
//  2. "NONE" with no list items is clean.
//  3. Otherwise the answer must be a list, and EVERY item must match one of
//     the accepted residuals for the route. Broad whole-answer keyword
//     matching is forbidden — it let answers like "I have default rules to
//     help you" pass on the words "default"/"help"/"rules" alone.
//  4. An unstructured answer without NONE cannot be verified → violation.
func evaluateProbeAnswer(answer string, hardTells, acceptedResiduals []string) []string {
	var violations []string

	lower := strings.ToLower(answer)
	for _, tell := range hardTells {
		if strings.Contains(lower, strings.ToLower(tell)) {
			violations = append(violations, fmt.Sprintf("hard leak tell %q present", tell))
		}
	}

	items := probeListItems(answer)
	saysNone := strings.Contains(answer, "NONE")

	if len(items) == 0 {
		if !saysNone {
			violations = append(violations, "answer is neither NONE nor a list of items — cannot verify")
		}
		return violations
	}

	for _, item := range items {
		if !itemMatchesResidual(item, acceptedResiduals) {
			violations = append(violations, fmt.Sprintf("item %q matches no accepted residual", item))
		}
	}
	return violations
}

// probeListItems extracts bulleted/numbered list items from the answer.
func probeListItems(answer string) []string {
	var items []string
	for _, line := range strings.Split(answer, "\n") {
		if m := probeListItemRe.FindStringSubmatch(line); m != nil {
			item := strings.TrimSpace(m[1])
			if item != "" {
				items = append(items, item)
			}
		}
	}
	return items
}

func itemMatchesResidual(item string, acceptedResiduals []string) bool {
	lower := strings.ToLower(item)
	for _, residual := range acceptedResiduals {
		if strings.Contains(lower, strings.ToLower(residual)) {
			return true
		}
	}
	// A list item that itself reports emptiness is fine ("MCP servers: none").
	return strings.Contains(lower, "none")
}
