package feishu

import (
	"encoding/json"
	"strings"
)

// splitMarkdown preserves raw input while balancing fenced code in each displayed fragment.
// [Main Request] 11 Cards: fit snapshots into continuation cards while carrying fenced-code language labels across
// fragments.
func splitMarkdown(text string, maximum int) []string {
	// Reserve room for a repeated language fence and closing delimiter.
	budget := maximum - 48
	var parts []string
	var current strings.Builder
	used := 0
	inside := false
	language := ""
	fence := "```"
	flush := func() {
		if current.Len() == 0 {
			return
		}
		piece := current.String()
		if inside {
			piece += "\n" + fence
		}
		parts = append(parts, piece)
		current.Reset()
		used = 0
		if inside {
			current.WriteString(fence + language + "\n")
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		trim := strings.TrimSpace(line)
		marker := ""
		if strings.HasPrefix(trim, "```") {
			marker = "```"
		} else if strings.HasPrefix(trim, "~~~") {
			marker = "~~~"
		}
		opening := !inside && marker != ""
		closing := inside && strings.HasPrefix(trim, fence) && strings.TrimSpace(strings.TrimPrefix(trim, fence)) == ""
		if opening || closing {
			// Parse each original line once; a split within a code line cannot become a delimiter.
			if used+len([]rune(line)) > budget {
				flush()
			}
			if opening {
				language = strings.TrimSpace(strings.TrimPrefix(trim, marker))
				if len([]rune(language)) > 32 {
					language = ""
				}
				fence = marker
				line = fence + language + "\n"
			} else {
				line = fence + "\n"
			}
			current.WriteString(line)
			used += len([]rune(line))
			inside = opening
			continue
		}
		remaining := []rune(line)
		for len(remaining) > 0 {
			if used == budget {
				flush()
			}
			n := min(len(remaining), budget-used)
			current.WriteString(string(remaining[:n]))
			used += n
			remaining = remaining[n:]
			if len(remaining) > 0 {
				flush()
			}
		}
	}
	if current.Len() > 0 {
		piece := current.String()
		if inside {
			piece += "\n" + fence
		}
		parts = append(parts, piece)
	}
	if len(parts) == 0 {
		return []string{""}
	}
	return parts
}

// renderCard delegates Markdown and fenced language rendering to the Feishu client.
func renderCard(text, status string) string {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"update_multi": true},
		"body": map[string]any{"elements": []any{
			map[string]any{"tag": "markdown", "content": text},
			map[string]any{"tag": "markdown", "content": "**Status:** " + status},
		}},
	}
	data, _ := json.Marshal(card)
	return string(data)
}
