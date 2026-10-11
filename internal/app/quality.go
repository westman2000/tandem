package app

import "strings"

func runModel(r Run, agent string) string {
	if model := r.LockedModels[agent]; model != "" {
		return model
	}
	return r.Models[agent]
}

func hasMarker(text, marker string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
}

// Two unsuccessful revisions after a baseline review. This is a reviewer
// judgment, not a numeric measure of document quality. Exact repetition is a
// fallback for CLI sessions which predate the structured progress instruction.
func stagnationReason(r Run) string {
	checks, documents := []string{}, []string{}
	for _, t := range r.Tasks {
		if t.Status != "done" {
			continue
		}
		if t.Stage == "verify" {
			checks = append(checks, t.Output)
		}
		if t.Stage == "revision" {
			documents = append(documents, strings.Join(strings.Fields(t.Output), " "))
		}
	}
	n := len(checks)
	if n < 3 {
		return ""
	}
	noProgress := func(s string) bool { return hasMarker(s, "[TANDEM_NO_PROGRESS]") && !hasMarker(s, "[TANDEM_PROGRESS]") }
	if noProgress(checks[n-1]) && noProgress(checks[n-2]) {
		return "Проверяющий агент два прохода подряд не видит существенного улучшения. Документ и замечания сохранены. Уточните спорные требования и продолжите доработку."
	}
	normal := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	d := len(documents)
	if d >= 3 && normal(checks[n-1]) == normal(checks[n-2]) && normal(checks[n-2]) == normal(checks[n-3]) && documents[d-1] == documents[d-2] && documents[d-2] == documents[d-3] {
		return "Три проверки и версии документа повторились без содержательных изменений. Уточните требования и продолжите доработку."
	}
	return ""
}
