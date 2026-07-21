package e2e

import (
	"strings"
	"testing"
)

func TestEvaluateProbeAnswer_None(t *testing.T) {
	if v := evaluateProbeAnswer("NONE", hardLeakTells, nil); len(v) != 0 {
		t.Fatalf("NONE should be clean, got %v", v)
	}
	if v := evaluateProbeAnswer("I was given nothing beyond my base prompt. NONE.", hardLeakTells, nil); len(v) != 0 {
		t.Fatalf("prose NONE should be clean, got %v", v)
	}
}

func TestEvaluateProbeAnswer_HardTell(t *testing.T) {
	answer := "NONE, though I can see the wp-mcp-router MCP server."
	v := evaluateProbeAnswer(answer, hardLeakTells, nil)
	if len(v) == 0 {
		t.Fatal("hard leak tell must fail even alongside NONE")
	}
}

func TestEvaluateProbeAnswer_AcceptedResidualItems(t *testing.T) {
	answer := "Beyond my base prompt I have:\n- built-in skill: imagegen\n- built-in skill: skill-creator\n- MCP servers: none"
	v := evaluateProbeAnswer(answer, hardLeakTells, []string{"imagegen", "skill-creator"})
	if len(v) != 0 {
		t.Fatalf("accepted residual items should be clean, got %v", v)
	}
}

func TestEvaluateProbeAnswer_UnrecognizedItemFails(t *testing.T) {
	answer := "- built-in skill: imagegen\n- CLAUDE.md from the home directory"
	v := evaluateProbeAnswer(answer, hardLeakTells, []string{"imagegen"})
	if len(v) != 1 || !strings.Contains(v[0], "CLAUDE.md") {
		t.Fatalf("unrecognized item must be a violation, got %v", v)
	}
}

// Regression: the old whole-answer keyword match passed any answer containing
// broad words like "rules", "help", or "default" — even contaminated prose.
func TestEvaluateProbeAnswer_BroadKeywordProseFails(t *testing.T) {
	answer := "I follow the default rules configured to help you, including your global instruction files."
	v := evaluateProbeAnswer(answer, hardLeakTells, []string{"rules", "help", "default"})
	if len(v) == 0 {
		t.Fatal("unstructured prose without NONE must not pass on broad keywords")
	}
}

func TestEvaluateProbeAnswer_NumberedListsParsed(t *testing.T) {
	answer := "1. account-level user rule: be terse\n2) account-level user rule: prefer bun"
	v := evaluateProbeAnswer(answer, hardLeakTells, []string{"user rule"})
	if len(v) != 0 {
		t.Fatalf("numbered list of accepted residuals should be clean, got %v", v)
	}
}
