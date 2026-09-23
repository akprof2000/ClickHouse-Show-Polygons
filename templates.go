package main

// Общие шаблоны слоёв. Раньше настройки лежали у каждого пользователя в
// config.json рядом с exe; теперь они хранятся на сервере и видны всем —
// один человек собрал набор слоёв, остальные загружают его в один клик.
//
// Хранилище намеренно простое: каталог с JSON-файлами по одному на шаблон.
// Шаблонов десятки, а не миллионы, база тут была бы лишней сущностью.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Template — именованный набор слоёв.
type Template struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Comment string          `json:"comment,omitempty"`
	Layers  json.RawMessage `json:"layers"`
	Created time.Time       `json:"created"`
	Updated time.Time       `json:"updated"`
}

type templateStore struct {
	dir string
	mu  sync.Mutex
}

func newTemplateStore(dataDir string) (*templateStore, error) {
	dir := filepath.Join(dataDir, "templates")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("каталог шаблонов %s: %w", dir, err)
	}
	return &templateStore{dir: dir}, nil
}

// slug делает из названия безопасное имя файла: без разделителей пути и
// без сюрпризов вроде "..". Кириллица сохраняется — имена файлов в UTF-8.
func slug(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r < 0x20:
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.Trim(strings.TrimSpace(b.String()), ".")
	if s == "" {
		s = "template"
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

func (s *templateStore) path(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("некорректный идентификатор шаблона")
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// List возвращает все шаблоны, свежие сверху.
func (s *templateStore) List() ([]Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue // битый файл не должен ронять список остальных
		}
		var t Template
		if err := json.Unmarshal(data, &t); err != nil {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func (s *templateStore) Get(id string) (Template, error) {
	p, err := s.path(id)
	if err != nil {
		return Template{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(p)
	if err != nil {
		return Template{}, err
	}
	var t Template
	if err := json.Unmarshal(data, &t); err != nil {
		return Template{}, fmt.Errorf("шаблон %s повреждён: %w", id, err)
	}
	return t, nil
}

// Save создаёт или перезаписывает шаблон. Идентификатор выводится из имени,
// поэтому сохранение под тем же именем обновляет существующий шаблон.
func (s *templateStore) Save(t Template) (Template, error) {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return Template{}, fmt.Errorf("у шаблона должно быть имя")
	}
	if len(t.Layers) == 0 || !json.Valid(t.Layers) {
		return Template{}, fmt.Errorf("в шаблоне нет корректного описания слоёв")
	}
	t.ID = slug(t.Name)
	p, err := s.path(t.ID)
	if err != nil {
		return Template{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Truncate(time.Second)
	t.Updated = now
	t.Created = now
	if old, err := os.ReadFile(p); err == nil {
		var prev Template
		if json.Unmarshal(old, &prev) == nil && !prev.Created.IsZero() {
			t.Created = prev.Created
		}
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return Template{}, err
	}
	// пишем через временный файл: прерванная запись не оставит обрезанный шаблон
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return Template{}, err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return Template{}, err
	}
	return t, nil
}

func (s *templateStore) Delete(id string) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.Remove(p)
}
