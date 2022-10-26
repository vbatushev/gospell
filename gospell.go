package gospell

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

// GoSpell is main struct
type GoSpell struct {
	Config    DictConfig
	Dict      map[string]struct{} // likely will contain some value later
	DB        *gorm.DB
	ireplacer *strings.Replacer // input conversion
	compounds []*regexp.Regexp
	splitter  *Splitter
}

// WordForm — структура для базы данных
type WordForm struct {
	ID   uint   `gorm:"primaryKey"`
	Word string `gorm:"index"`
	Lang string
	Case WordCase
}

// Preferences - настройки, хранящиеся в базе данных
type Preferences struct {
	ID   uint `gorm:"primaryKey"`
	Dict string
}

var (
	reHyphenAndSymbol = regexp.MustCompile(`\-[\p{L}\-]`)
	rePunctuation     = regexp.MustCompile(`[!\?\:;,\.—«»\(\)\[\]\<\>§…]`)
	reLatinNum        = regexp.MustCompile(`^M{0,3}(CM|CD|D?C{0,3})?(XC|XL|L?X{0,3})?(IX|IV|V?I{0,3})?$`)
)

// InputConversion does any character substitution before checking
//
//	This is based on the ICONV stanza
func (s *GoSpell) InputConversion(raw []byte) string {
	sraw := string(raw)
	if s.ireplacer == nil {
		return sraw
	}
	return s.ireplacer.Replace(sraw)
}

// Split a text into Words
func (s *GoSpell) Split(text string) []string {
	return s.splitter.Split(text)
}

// AddWordRaw adds a single word to the internal dictionary without modifications
// returns true if added
// return false is already exists
func (s *GoSpell) AddWordRaw(word string) bool {
	_, ok := s.Dict[word]
	if ok {
		// already exists
		return false
	}
	s.Dict[word] = struct{}{}
	return true
}

// AddWordListFile reads in a word list file
func (s *GoSpell) AddWordListFile(name string) ([]string, error) {
	fd, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	return s.AddWordList(fd)
}

// AddWordList adds basic word lists, just one word per line
//
//	Assumed to be in UTF-8
//
// TODO: hunspell compatible with "*" prefix for forbidden words
// and affix support
// returns list of duplicated words and/or error
func (s *GoSpell) AddWordList(r io.Reader) ([]string, error) {
	var duplicates []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 || line == "#" {
			continue
		}
		for _, word := range CaseVariations(line, CaseStyle(line)) {
			if !s.AddWordRaw(word) {
				duplicates = append(duplicates, word)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return duplicates, err
	}
	return duplicates, nil
}

// Spell checks to see if a given word is in the internal dictionaries
// TODO: add multiple dictionaries
func (s *GoSpell) Spell(word string) bool {
	if s.DB == nil {
		_, ok := s.Dict[word]
		if ok {
			return true
		}
	} else {
		var wf WordForm
		s.DB.Where("word = ?", strings.ToLower(word)).First(&wf)
		if wf.Case == Mixed {
			return true
		}
		if wf.Case == AllUpper && word == strings.ToUpper(word) {
			return true
		}
		if wf.Case == Title && (word == strings.ToUpper(word) || word == strings.ToTitle(word)) {
			return true
		}
	}
	if isNumber(word) {
		return true
	}
	if isNumberHex(word) {
		return true
	}

	if isNumberBinary(word) {
		return true
	}

	if isLatinNumber(word) {
		return true
	}

	if isHash(word) {
		return true
	}

	// check compounds
	for _, pat := range s.compounds {
		if pat.MatchString(word) {
			return true
		}
	}

	// Maybe a word with units? e.g. 100GB
	units := isNumberUnits(word)
	if units != "" {
		if s.DB == nil {
			// dictionary appears to have list of units
			if _, ok := s.Dict[units]; ok {
				return true
			}
		} else {
			r := s.DB.First(&WordForm{
				Word: units,
			})
			if !errors.Is(r.Error, gorm.ErrRecordNotFound) {
				return true
			}
		}
	}

	// if camelCase and each word e.g. "camel" "Case" is know
	// then the word is considered known
	if chunks := splitCamelCase(word); len(chunks) > 0 {
		if false {
			for _, chunk := range chunks {
				if s.DB == nil {
					if _, ok := s.Dict[chunk]; !ok {
						return false
					}
				} else {
					r := s.DB.First(&WordForm{
						Word: chunk,
					})
					if errors.Is(r.Error, gorm.ErrRecordNotFound) {
						return false
					}
				}
			}
		}
		return true
	}

	return false
}

// NewGoSpellReader создает GoSpell из файлов Huspell, переданных, как io.Reader
// Если db передано не как nil, собирается таблица словоформ,
func NewGoSpellReader(aff, dic io.Reader, db *gorm.DB, lang string) (*GoSpell, error) {
	affix, err := NewDictConfig(aff)
	if err != nil {
		return nil, err
	}

	gs := GoSpell{
		compounds: make([]*regexp.Regexp, 0, len(affix.CompoundRule)),
		splitter:  NewSplitter(affix.WordChars),
	}

	words := []string{}
	wordForms := []WordForm{}

	scanner := bufio.NewScanner(dic)

	if !scanner.Scan() {
		return nil, scanner.Err()
	}
	line := scanner.Text()
	i, err := strconv.ParseInt(line, 10, 64)
	if err != nil {
		return nil, err
	}

	if db == nil {
		gs.Dict = make(map[string]struct{}, i*5)
	}

	for scanner.Scan() {
		line := scanner.Text()
		words, err = affix.Expand(line, words)
		if err != nil {
			return nil, fmt.Errorf("Unable to process %q: %s", line, err)
		}

		if len(words) == 0 {
			continue
		}

		style := CaseStyle(words[0])
		for _, word := range words {
			if db != nil {
				st := style
				if st != Mixed && st != AllUpper && st != Title {
					st = Mixed
				}
				wordForms = append(wordForms, WordForm{
					Word: strings.ToLower(word),
					Lang: lang,
					Case: st,
				})
			} else {
				for _, wordform := range CaseVariations(word, style) {
					gs.Dict[wordform] = struct{}{}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	for _, compoundRule := range affix.CompoundRule {
		pattern := "^"
		for _, key := range compoundRule {
			switch key {
			case '(', ')', '+', '?', '*':
				pattern = pattern + string(key)
			default:
				groups := affix.CompoundMap[key]
				pattern = pattern + "(" + strings.Join(groups, "|") + ")"
			}
		}
		pattern = pattern + "$"
		pat, err := regexp.Compile(pattern)
		if err != nil {
			log.Printf("REGEXP FAIL= %q %s", pattern, err)
		} else {
			gs.compounds = append(gs.compounds, pat)
		}
	}

	if len(affix.IconvReplacements) > 0 {
		gs.ireplacer = strings.NewReplacer(affix.IconvReplacements...)
	}

	if db != nil {
		result := db.Create(&wordForms)
		if result.Error != nil {
			return nil, result.Error
		}

		if cfg, err := json.Marshal(gs.Dict); err == nil {
			var configs []Preferences
			configs = append(configs, Preferences{
				Dict: string(cfg),
			})
			result = db.Create(&configs)
			if result.Error != nil {
				return nil, result.Error
			}
		}

	}
	gs.DB = db
	return &gs, nil
}

// NewGoSpell создает новый GoSpell из файлов AFF, DIC Hunspell
func NewGoSpell(affFile, dicFile string) (*GoSpell, error) {
	aff, err := os.Open(affFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to open aff: %s", err)
	}
	defer aff.Close()
	dic, err := os.Open(dicFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to open dic: %s", err)
	}
	defer dic.Close()
	h, err := NewGoSpellReader(aff, dic, nil, "")
	return h, err
}

// NewGoSpellDBForce создает из файлов AFF, DIC Hunspell
// и складывает всё в базу данных, указанную в dbFile
func NewGoSpellDBForce(affFile, dicFile, dbFile string, config *gorm.Config) (*GoSpell, error) {
	db := createTable(dbFile, true, config)

	aff, err := os.Open(affFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to open aff: %s", err)
	}
	defer aff.Close()
	dic, err := os.Open(dicFile)
	if err != nil {
		return nil, fmt.Errorf("Unable to open dic: %s", err)
	}
	defer dic.Close()

	var lang string
	if df, err := os.Stat(dicFile); err == nil {
		lang = fileNameWithoutExtTrimSuffix(df.Name())
	}

	h, err := NewGoSpellReader(aff, dic, db, lang)
	return h, err
}

// Получение имени файла без расширения
func fileNameWithoutExtTrimSuffix(fileName string) string {
	return strings.TrimSuffix(fileName, filepath.Ext(fileName))
}

func createTable(dbFile string, force bool, config *gorm.Config) *gorm.DB {
	if force {
		if _, err := os.Stat(dbFile); errors.Is(err, os.ErrNotExist) {
			if err := os.Remove(dbFile); err != nil {
				return nil
			}
		}
		if _, err := os.Create(dbFile); err != nil {
			return nil
		}
	}

	if config == nil {
		config = &gorm.Config{
			CreateBatchSize:        1000,
			SkipDefaultTransaction: true,
			PrepareStmt:            true,
		}
	}

	db, err := gorm.Open(sqlite.Open(dbFile), config)
	if err != nil {
		return nil
	}

	if force {
		err = db.Migrator().CreateTable(&WordForm{})
		if err != nil {
			return nil
		}
		err = db.Migrator().CreateTable(&Preferences{})
		if err != nil {
			return nil
		}
	}
	db.Use(
		dbresolver.Register(dbresolver.Config{}).
			SetConnMaxIdleTime(time.Hour).
			SetConnMaxLifetime(24 * time.Hour).
			SetMaxIdleConns(100).
			SetMaxOpenConns(200),
	)

	return db
}

// NewGoSpellDB создает GoSpell с использованием указанной в пути базы данных
func NewGoSpellDB(dbFile string, config *gorm.Config) (*GoSpell, error) {
	db := createTable(dbFile, false, config)
	h, err := NewGoSpellDBReader(db)
	return h, err
}

// NewGoSpellDBReader создает GoSpell с использованием указанной базы данных
func NewGoSpellDBReader(db *gorm.DB) (*GoSpell, error) {
	var prefs Preferences
	db.First(&prefs)
	if prefs.Dict == "" {
		return nil, errors.New("Not found Dict in preferences")
	}
	var affix *DictConfig
	json.Unmarshal([]byte(prefs.Dict), &affix)
	gs := GoSpell{
		compounds: make([]*regexp.Regexp, 0, len(affix.CompoundRule)),
		splitter:  NewSplitter(affix.WordChars),
	}

	for _, compoundRule := range affix.CompoundRule {
		pattern := "^"
		for _, key := range compoundRule {
			switch key {
			case '(', ')', '+', '?', '*':
				pattern = pattern + string(key)
			default:
				groups := affix.CompoundMap[key]
				pattern = pattern + "(" + strings.Join(groups, "|") + ")"
			}
		}
		pattern = pattern + "$"
		pat, err := regexp.Compile(pattern)
		if err != nil {
			log.Printf("REGEXP FAIL= %q %s", pattern, err)
		} else {
			gs.compounds = append(gs.compounds, pat)
		}
	}

	if len(affix.IconvReplacements) > 0 {
		gs.ireplacer = strings.NewReplacer(affix.IconvReplacements...)
	}

	gs.DB = db
	return &gs, nil
}
