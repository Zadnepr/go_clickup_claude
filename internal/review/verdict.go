// Package review запускает /spec и /review силами claude -p и разбирает
// контракт вердикта. Смысл ревью не анализируется здесь: Go только парсит
// одну служебную строку вида "ИТОГ: критичных=N важных=N минор=N статус=X".
package review

import "regexp"

// Возможные статусы вердикта.
const (
	StatusPass    = "pass"
	StatusFail    = "fail"
	StatusBlocked = "blocked"
)

// Verdict — разобранный контракт вердикта команды /review.
type Verdict struct {
	Critical  int
	Important int
	Minor     int
	Status    string // pass | fail | blocked
	Raw       string // исходная строка "ИТОГ: ...", если была найдена
}

// verdictLineRe — единственное регулярное выражение, разрешённое ТЗ: разбор
// служебной строки вердикта, а не содержимого ревью.
var verdictLineRe = regexp.MustCompile(`(?m)^ИТОГ:\s*критичных=(\d+)\s+важных=(\d+)\s+минор=(\d+)\s+статус=(pass|fail|blocked)\s*$`)

// ParseVerdict находит последнее вхождение строки "ИТОГ: " в тексте ревью и
// разбирает её. Если строки нет или она не разбирается, прогон считается
// blocked — молча ронять результат нельзя.
func ParseVerdict(output string) Verdict {
	matches := verdictLineRe.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return Verdict{Status: StatusBlocked}
	}

	last := matches[len(matches)-1]
	return Verdict{
		Critical:  atoi(last[1]),
		Important: atoi(last[2]),
		Minor:     atoi(last[3]),
		Status:    last[4],
		Raw:       last[0],
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
