package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SessionLogPaths returns one diagnostic file per generation, in numeric order.
// NDJSON takes precedence over legacy JSONL when both formats exist.
func SessionLogPaths(dir, sessionID string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	if sessionID == "" || filepath.Base(sessionID) != sessionID {
		return nil, fmt.Errorf("invalid session log ID")
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	generations := map[int64]string{}
	for _, entry := range entries {
		name := entry.Name()
		ext := filepath.Ext(name)
		if ext != logFileExt && ext != legacyLogFileExt {
			continue
		}
		stem := strings.TrimSuffix(name, ext)
		number := int64(1)
		if stem != sessionID {
			suffix, ok := strings.CutPrefix(stem, sessionID+".g")
			if !ok {
				continue
			}
			parsed, err := strconv.ParseInt(suffix, 10, 64)
			if err != nil || parsed < 2 || strconv.FormatInt(parsed, 10) != suffix {
				continue
			}
			number = parsed
		}
		if previous := generations[number]; previous == "" || ext == logFileExt {
			generations[number] = filepath.Join(dir, name)
		}
	}
	numbers := make([]int64, 0, len(generations))
	for number := range generations {
		numbers = append(numbers, number)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	paths := make([]string, 0, len(numbers))
	for _, number := range numbers {
		paths = append(paths, generations[number])
	}
	return paths, nil
}
