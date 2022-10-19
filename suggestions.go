package gospell

import (
	"strings"
)

// SpellWithSuggestions — проверка слова и получение для него возможных замен
func (s *GoSpell) SpellWithSuggestions(word string) (suggestions []string) {
	if s.Spell(word) {
		return
	}
	return s.GetSuggestions(word)
}

// GetSuggestions - Поиск возможных подстановок
func (s *GoSpell) GetSuggestions(word string) []string {
	if s.Spell(word) || s.Spell(strings.ToLower(word)) {
		return []string{}
	}

	if isLatinNumber(word) || isInitial(word) {
		return []string{}
	}

	sqlWords := []string{}
	letters := strings.Split(word, "")
	for i := range letters {
		s := "`word` LIKE '" + substr(word, 0, i) + "_" + substr(word, i+1, len([]rune(word))) + "'"
		sqlWords = append(sqlWords, s)
	}
	sqlWords = append(sqlWords, "`word` LIKE '"+substr(word, 1, len([]rune(word)))+"'")
	if reHyphenAndSymbol.MatchString(word) {
		v := reHyphenAndSymbol.ReplaceAllString(word, "")
		sqlWords = append(sqlWords, "`word` LIKE '"+v+"'")
	}

	var founds []WordForm
	condition := strings.Join(sqlWords, " OR ")
	result := s.DB.Where(condition).Select("word").Order("word asc").Find(&founds)

	variants := []string{}
	if result.Error == nil {
		for _, suggestion := range founds {
			variants = append(variants, suggestion.Word)
		}
	}

	return variants
}

type onlyWord struct {
	ID   uint
	Word string
}

// Получение подстроки из строки
func substr(input string, start int, length int) string {
	asRunes := []rune(input)

	if start >= len(asRunes) {
		return ""
	}

	if start+length > len(asRunes) {
		length = len(asRunes) - start
	}

	return string(asRunes[start : start+length])
}
