package review

import "testing"

func TestParseVerdict_Pass(t *testing.T) {
	text := "Всё хорошо, диф чистый.\n\nИТОГ: критичных=0 важных=1 минор=3 статус=pass\n"
	v := ParseVerdict(text)
	if v.Status != StatusPass || v.Critical != 0 || v.Important != 1 || v.Minor != 3 {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestParseVerdict_Fail(t *testing.T) {
	text := "Найдены проблемы.\nИТОГ: критичных=2 важных=0 минор=0 статус=fail"
	v := ParseVerdict(text)
	if v.Status != StatusFail || v.Critical != 2 {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestParseVerdict_TakesLastOccurrence(t *testing.T) {
	text := "ИТОГ: критичных=5 важных=5 минор=5 статус=fail\n" +
		"немного текста между строками\n" +
		"ИТОГ: критичных=0 важных=0 минор=0 статус=pass\n"
	v := ParseVerdict(text)
	if v.Status != StatusPass || v.Critical != 0 {
		t.Errorf("expected last occurrence to win, got: %+v", v)
	}
}

func TestParseVerdict_Blocked(t *testing.T) {
	text := "Ветка не найдена, диф получить не удалось.\nИТОГ: критичных=0 важных=0 минор=0 статус=blocked"
	v := ParseVerdict(text)
	if v.Status != StatusBlocked {
		t.Errorf("expected blocked status, got: %+v", v)
	}
}

func TestParseVerdict_MissingLine(t *testing.T) {
	text := "Ревью прошло успешно, замечаний нет."
	v := ParseVerdict(text)
	if v.Status != StatusBlocked {
		t.Errorf("expected blocked when ИТОГ line is missing, got: %+v", v)
	}
	if v.Raw != "" {
		t.Errorf("expected empty Raw, got: %q", v.Raw)
	}
}

func TestParseVerdict_MalformedLine(t *testing.T) {
	cases := []string{
		"ИТОГ: критичных=abc важных=1 минор=0 статус=pass",
		"ИТОГ: критичных=1 важных=1 статус=pass",
		"ИТОГ: критичных=1 важных=1 минор=1 статус=unknown",
		"итог: критичных=0 важных=0 минор=0 статус=pass",
	}
	for _, c := range cases {
		v := ParseVerdict(c)
		if v.Status != StatusBlocked {
			t.Errorf("input %q: expected blocked for malformed line, got %+v", c, v)
		}
	}
}

func TestParseVerdict_EmptyOutput(t *testing.T) {
	v := ParseVerdict("")
	if v.Status != StatusBlocked {
		t.Errorf("expected blocked for empty output, got: %+v", v)
	}
}
