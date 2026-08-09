package runtime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeStreamFixturesAreValidNDJSON(t *testing.T) {
	// STR-001: fixtures are executable protocol contracts, not inert examples.
	t.Parallel()

	fixtures := []string{
		"claude/input.ndjson",
		"claude/output.ndjson",
		"codex/input.ndjson",
		"codex/output.ndjson",
	}
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			lines := readNativeFixture(t, fixture)
			if len(lines) == 0 {
				t.Fatal("fixture must contain at least one record")
			}
			for index, line := range lines {
				var record map[string]json.RawMessage
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatalf("record %d is not JSON: %v", index+1, err)
				}
				if len(record) == 0 {
					t.Fatalf("record %d is empty", index+1)
				}
			}
		})
	}
}

func TestNativeStreamFixturesCoverRecoveryContract(t *testing.T) {
	t.Parallel()

	assertFixtureContains(t, "claude/input.ndjson", []string{
		`"type":"user"`,
		`"subtype":"interrupt"`,
		`"type":"control_response"`,
	})
	assertFixtureContains(t, "claude/output.ndjson", []string{
		`"subtype":"init"`,
		`"type":"text_delta"`,
		`"type":"tool_use"`,
		`"type":"tool_result"`,
		`"usage"`,
		`"subtype":"can_use_tool"`,
		`"is_error":true`,
		`"subtype":"success"`,
	})
	assertFixtureContains(t, "codex/input.ndjson", []string{
		`"method":"initialize"`,
		`"method":"thread/start"`,
		`"method":"thread/resume"`,
		`"method":"turn/start"`,
		`"method":"turn/steer"`,
		`"method":"turn/interrupt"`,
		`"decision":"accept"`,
	})
	assertFixtureContains(t, "codex/output.ndjson", []string{
		`"method":"thread/started"`,
		`"method":"item/agentMessage/delta"`,
		`"method":"item/started"`,
		`"method":"item/completed"`,
		`requestApproval`,
		`"method":"error"`,
		`"method":"turn/completed"`,
		`"usage"`,
	})
}

func readNativeFixture(t *testing.T, name string) [][]byte {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "native-streams", name)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture %s: %v", name, err)
	}
	defer file.Close()

	var lines [][]byte
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return lines
}

func assertFixtureContains(t *testing.T, name string, fragments []string) {
	t.Helper()

	var joined []byte
	for _, line := range readNativeFixture(t, name) {
		joined = append(joined, line...)
		joined = append(joined, '\n')
	}
	for _, fragment := range fragments {
		if !bytes.Contains(joined, []byte(fragment)) {
			t.Errorf("fixture %s missing contract fragment %s", name, fragment)
		}
	}
}
