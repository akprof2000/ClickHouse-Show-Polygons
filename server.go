package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	tokenCookie  = "chviewer_token"
	maxRequest   = 1 << 20 // тело запроса от браузера: заведомо с запасом
	defaultLimit = 3000
)

type Server struct {
	cfg     Config
	ch      *chClient
	tpl     *templateStore
	static  fs.FS
	token   string
	version string

	cacheMu sync.Mutex
	caches  map[string]*layerCache
}

func NewServer(cfg Config, ch *chClient, tpl *templateStore, static fs.FS, version string) *Server {
	return &Server{
		cfg: cfg, ch: ch, tpl: tpl, static: static, version: version,
		token:  cfg.AuthToken(),
		caches: map[string]*layerCache{},
	}
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.Handle("/", s.staticHeaders(http.FileServer(http.FS(s.static))))

	m.HandleFunc("/api/info", s.api(s.handleInfo, false))
	m.HandleFunc("/api/login", s.api(s.handleLogin, false))
	m.HandleFunc("/api/logout", s.api(s.handleLogout, false))

	m.HandleFunc("/api/query", s.api(s.handleQuery, true))
	m.HandleFunc("/api/objects", s.api(s.handleObjects, true))
	m.HandleFunc("/api/search", s.api(s.handleSearch, true))
	m.HandleFunc("/api/templates", s.api(s.handleTemplates, true))
	m.HandleFunc("/api/templates/", s.api(s.handleTemplateItem, true))
	return m
}

// ---------- общая обвязка запросов ----------

// api оборачивает обработчик: заголовки, защита от межсайтовых запросов и,
// если нужно, проверка токена. Проверка на localhost, которая была в
// настольной версии, здесь не нужна и вредна: сервер слушает сеть.
func (s *Server) api(next http.HandlerFunc, authRequired bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")

		// Cookie сессии помечена SameSite=Strict, но полагаться только на
		// неё мало: проверяем происхождение запроса и требуем JSON, который
		// обычная HTML-форма со стороннего сайта поставить не может.
		if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
			log.Printf("SECURITY %s отклонён: межсайтовый Origin %q", r.URL.Path, o)
			writeJSONErr(w, http.StatusForbidden, "cross-origin")
			return
		}
		if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
			log.Printf("SECURITY %s отклонён: Sec-Fetch-Site=%q", r.URL.Path, sf)
			writeJSONErr(w, http.StatusForbidden, "cross-origin")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			ct := r.Header.Get("Content-Type")
			if i := strings.IndexByte(ct, ';'); i >= 0 {
				ct = ct[:i]
			}
			if strings.TrimSpace(strings.ToLower(ct)) != "application/json" {
				writeJSONErr(w, http.StatusUnsupportedMediaType, "нужен Content-Type: application/json")
				return
			}
		}
		if authRequired && s.token != "" && !s.tokenOK(r) {
			writeJSONErr(w, http.StatusUnauthorized, "нужен вход")
			return
		}
		next(w, r)
	}
}

func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

func (s *Server) tokenOK(r *http.Request) bool {
	got := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie(tokenCookie); err == nil {
		got = c.Value
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeErr(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg})
}

func writeErrSQL(w http.ResponseWriter, msg, sql string) {
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg, "sql": sql})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxRequest)).Decode(v)
}

// статика: запрещаем угадывание типа и лишние сторонние ресурсы
func (s *Server) staticHeaders(next http.Handler) http.Handler {
	csp := buildCSP(s.cfg.Basemaps)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Не no-referrer: тайл-серверы (OpenStreetMap прямо по своим правилам)
		// требуют Referer у запросов со страниц и без него отвечают 403 —
		// на проде за общим NAT это срабатывает сразу. strict-origin-when-
		// cross-origin отдаёт чужим сайтам только адрес сервера, без пути и
		// параметров, и ничего не отдаёт при переходе с HTTPS на HTTP.
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// buildCSP собирает политику безопасности страницы. Картинки и запросы
// разрешены на любой https-адрес (подложки по https подключаются без правки
// политики) и дополнительно — на адреса подложек из конфигурации. Последнее
// нужно для внутренних тайл-серверов по обычному http: без этого браузер
// молча не загрузит такую подложку.
func buildCSP(basemaps []Basemap) string {
	extra := map[string]bool{}
	for _, b := range basemaps {
		for _, t := range b.Tiles {
			// в шаблоне бывают {z}/{x}/{y} — url.Parse их не любит
			t = strings.NewReplacer("{z}", "0", "{x}", "0", "{y}", "0", "{s}", "a").Replace(t)
			u, err := url.Parse(t)
			if err != nil || u.Host == "" || u.Scheme != "http" {
				continue // https уже разрешён целиком
			}
			extra[u.Scheme+"://"+u.Host] = true
		}
	}
	origins := make([]string, 0, len(extra))
	for o := range extra {
		origins = append(origins, o)
	}
	sort.Strings(origins)
	add := ""
	if len(origins) > 0 {
		add = " " + strings.Join(origins, " ")
	}
	return "default-src 'self'; img-src 'self' data: blob: https:" + add + "; " +
		"style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-eval'; " +
		"connect-src 'self' https:" + add + "; worker-src 'self' blob:; frame-ancestors 'none'"
}

// ---------- вход ----------

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"title":       s.cfg.Title,
		"version":     s.version,
		"auth":        s.token != "",
		"logged_in":   s.token == "" || s.tokenOK(r),
		"clickhouse":  s.ch.Addrs(),
		"credentials": credSource(s.cfg.ClickHouse),
		"basemaps":    s.cfg.Basemaps,
	})
}

func credSource(c CHConfig) string {
	if c.PAM.Enabled() {
		return "PAM " + c.PAM.Secret
	}
	return "конфигурация сервера"
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONErr(w, http.StatusMethodNotAllowed, "method")
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "некорректный запрос")
		return
	}
	if s.token == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth": false})
		return
	}
	if subtle.ConstantTimeCompare([]byte(in.Token), []byte(s.token)) != 1 {
		log.Printf("SECURITY неудачный вход с %s", r.RemoteAddr)
		time.Sleep(500 * time.Millisecond) // притормаживаем перебор
		writeJSONErr(w, http.StatusUnauthorized, "неверный токен")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: tokenCookie, Value: s.token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 30 * 24 * 3600,
	})
	log.Printf("вход выполнен с %s", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- шаблоны слоёв ----------

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.tpl.List()
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"templates": list})
	case http.MethodPost:
		var t Template
		if err := readJSON(r, &t); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "некорректный запрос")
			return
		}
		saved, err := s.tpl.Save(t)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("ШАБЛОН сохранён: %s", saved.ID)
		writeJSON(w, http.StatusOK, saved)
	default:
		writeJSONErr(w, http.StatusMethodNotAllowed, "method")
	}
}

func (s *Server) handleTemplateItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/templates/")
	switch r.Method {
	case http.MethodGet:
		t, err := s.tpl.Get(id)
		if err != nil {
			writeJSONErr(w, http.StatusNotFound, "шаблон не найден")
			return
		}
		writeJSON(w, http.StatusOK, t)
	case http.MethodDelete:
		if err := s.tpl.Delete(id); err != nil {
			writeJSONErr(w, http.StatusNotFound, "шаблон не найден")
			return
		}
		log.Printf("ШАБЛОН удалён: %s", id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSONErr(w, http.StatusMethodNotAllowed, "method")
	}
}

// ---------- произвольный запрос ----------

type queryReq struct {
	SQL string `json:"sql"`
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONErr(w, http.StatusMethodNotAllowed, "method")
		return
	}
	var q queryReq
	if err := readJSON(r, &q); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "некорректный запрос")
		return
	}
	log.Printf("QUERY -> %s | полный SQL:\n%s", s.ch.Addrs(), q.SQL)
	start := time.Now()
	rows, err := s.ch.Query(r.Context(), q.SQL)
	dur := time.Since(start).Round(time.Millisecond)
	if err != nil {
		log.Printf("ERROR QUERY за %s: %s", dur, trimSQL(err.Error(), 500))
		writeErr(w, err.Error())
		return
	}
	log.Printf("QUERY OK: %d строк за %s", len(rows), dur)
	// ответ — данные, не документ: вместе с nosniff это не даёт браузеру
	// истолковать его как страницу
	w.Header().Set("Content-Disposition", "attachment")
	writeJSON(w, http.StatusOK, map[string]any{"rows": len(rows), "data": rows})
}

// ---------- объекты с кэшем на стороне сервера ----------

type objectsReq struct {
	Table  string    `json:"table"`
	Geo    string    `json:"geo"`
	Bbox   []float64 `json:"bbox"` // [w,s,e,n]
	Limit  int       `json:"limit"`
	MinDeg float64   `json:"min_deg"` // объекты мельче этого размера не грузим
	Reset  bool      `json:"reset"`
}

func (s *Server) handleObjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONErr(w, http.StatusMethodNotAllowed, "method")
		return
	}
	var q objectsReq
	if err := readJSON(r, &q); err != nil || len(q.Bbox) != 4 || q.Table == "" || q.Geo == "" {
		writeJSONErr(w, http.StatusBadRequest, "некорректный запрос")
		return
	}
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}
	view := rect{W: q.Bbox[0], S: q.Bbox[1], E: q.Bbox[2], N: q.Bbox[3]}

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	key := q.Table + "|" + q.Geo
	cache, ok := s.caches[key]
	if !ok || q.Reset {
		if ok {
			log.Printf("CACHE сброс слоя %q; было объектов: %d", key, len(cache.objs))
		}
		cache = &layerCache{objs: map[string]cachedObj{}}
		s.caches[key] = cache
	}

	// экран уже внутри области, загруженной с достаточной детализацией — БД не трогаем
	fromCacheOnly := false
	for _, c := range cache.rects {
		if c.contains(view) && c.minDeg <= q.MinDeg*1.0001 {
			fromCacheOnly = true
			break
		}
	}

	usedSQL := ""
	if !fromCacheOnly {
		// берём с запасом +30%, чтобы мелкие сдвиги карты не порождали запросов
		padX, padY := (view.E-view.W)*0.3, (view.N-view.S)*0.3
		req := rect{W: view.W - padX, S: view.S - padY, E: view.E + padX, N: view.N + padY}

		geo, table := sqlIdent(q.Geo), q.Table
		// исключаем то, что уже загружено с нужной детализацией
		skip := ""
		for _, c := range cache.rects {
			if c.minDeg > q.MinDeg*1.0001 {
				continue // область грузилась грубее — мелкие объекты оттуда ещё не читались
			}
			skip += fmt.Sprintf("\n  AND NOT (arrayMin(xs) <= %g AND arrayMax(xs) >= %g AND arrayMin(ys) <= %g AND arrayMax(ys) >= %g)",
				c.E, c.W, c.N, c.S)
		}
		if q.MinDeg > 0 {
			skip += fmt.Sprintf("\n  AND (arrayMax(xs) - arrayMin(xs) >= %g OR arrayMax(ys) - arrayMin(ys) >= %g)",
				q.MinDeg, q.MinDeg)
		}
		usedSQL = fmt.Sprintf(`WITH flatten(flatten(%s)) AS pts,
     arrayMap(p -> p.1, pts) AS xs,
     arrayMap(p -> p.2, pts) AS ys
SELECT *, cityHash64(toString(%s)) AS __id
FROM %s
WHERE arrayMin(xs) <= %g AND arrayMax(xs) >= %g
  AND arrayMin(ys) <= %g AND arrayMax(ys) >= %g
  %s
LIMIT %d`, geo, geo, table, req.E, req.W, req.N, req.S, skip, q.Limit)

		log.Printf("OBJECTS | кэш: %d объектов, %d областей | полный SQL:\n%s",
			len(cache.objs), len(cache.rects), usedSQL)

		data, err := s.ch.Query(r.Context(), usedSQL)
		if err != nil {
			writeErrSQL(w, err.Error(), usedSQL)
			return
		}
		added := 0
		for _, row := range data {
			id, _ := row["__id"].(string)
			if id == "" {
				id = fmt.Sprint(row["__id"])
			}
			if _, ok := cache.objs[id]; ok {
				continue
			}
			bb, ok := geomBBox(row[q.Geo])
			if !ok {
				continue
			}
			cache.objs[id] = cachedObj{bb: bb, row: row}
			added++
		}
		// область считаем покрытой, только если лимит не отрезал хвост
		if len(data) < q.Limit {
			already := false
			for _, c := range cache.rects {
				if c.contains(req) && c.minDeg <= q.MinDeg*1.0001 {
					already = true
					break
				}
			}
			if !already {
				cache.rects = append(cache.rects, coveredRect{rect: req, minDeg: q.MinDeg})
				if len(cache.rects) > 64 {
					cache.rects = cache.rects[len(cache.rects)-64:]
				}
			}
		}
		log.Printf("OBJECTS OK: добавлено %d новых, в кэше %d", added, len(cache.objs))
	} else {
		log.Printf("OBJECTS из кэша: экран покрыт, БД не запрашивалась (в кэше %d объектов)", len(cache.objs))
	}

	out := make([]map[string]any, 0, 256)
	for _, o := range cache.objs {
		if !o.bb.intersects(view) {
			continue
		}
		if q.MinDeg > 0 && (o.bb.E-o.bb.W) < q.MinDeg && (o.bb.N-o.bb.S) < q.MinDeg {
			continue // слишком мелкий для текущего масштаба
		}
		out = append(out, o.row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows": len(out), "cached": fromCacheOnly, "sql": usedSQL,
		"total_cached": len(cache.objs), "data": out,
	})
}

// ---------- контекстный поиск по всем полям таблицы ----------

type searchReq struct {
	Table string `json:"table"`
	Geo   string `json:"geo"`
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONErr(w, http.StatusMethodNotAllowed, "method")
		return
	}
	var q searchReq
	if err := readJSON(r, &q); err != nil || strings.TrimSpace(q.Query) == "" || q.Table == "" {
		writeJSONErr(w, http.StatusBadRequest, "некорректный запрос")
		return
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}

	// структура таблицы: ищем по всем колонкам, кроме геометрии
	descSQL := "DESCRIBE TABLE " + q.Table
	desc, err := s.ch.Query(r.Context(), descSQL)
	if err != nil {
		writeErrSQL(w, err.Error(), descSQL)
		return
	}
	var cols, strs []string
	for _, c := range desc {
		name, _ := c["name"].(string)
		if name == "" || name == q.Geo {
			continue
		}
		cols = append(cols, sqlIdent(name))
		strs = append(strs, "toString("+sqlIdent(name)+")")
	}
	if len(cols) == 0 {
		writeErrSQL(w, "в таблице нет колонок для поиска (кроме геометрии)", "")
		return
	}

	esc := strings.ReplaceAll(strings.ReplaceAll(q.Query, `\`, `\\`), `'`, `\'`)
	geo := sqlIdent(q.Geo)
	sql := fmt.Sprintf(`WITH flatten(flatten(%s)) AS pts,
     arrayMap(p -> p.1, pts) AS xs,
     arrayMap(p -> p.2, pts) AS ys
SELECT %s,
  cityHash64(toString(%s)) AS __id,
  arrayMin(xs) AS __w, arrayMax(xs) AS __e, arrayMin(ys) AS __s, arrayMax(ys) AS __n
FROM %s
WHERE positionCaseInsensitiveUTF8(arrayStringConcat([%s], ' '), '%s') > 0
LIMIT %d`,
		geo, strings.Join(cols, ", "), geo, q.Table, strings.Join(strs, ", "), esc, q.Limit)

	log.Printf("SEARCH | таблица %s | запрос %q", q.Table, q.Query)
	data, err := s.ch.Query(r.Context(), sql)
	if err != nil {
		writeErrSQL(w, err.Error(), sql)
		return
	}
	log.Printf("SEARCH OK: %d совпадений в %s", len(data), q.Table)
	writeJSON(w, http.StatusOK, map[string]any{"rows": len(data), "sql": sql, "data": data})
}

// Run поднимает HTTP(S)-сервер и корректно останавливает его по сигналу.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		if s.cfg.TLS.CertFile != "" {
			log.Printf("Сервер слушает https://%s/", s.cfg.Listen)
			errc <- srv.ListenAndServeTLS(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
			return
		}
		log.Printf("Сервер слушает http://%s/", s.cfg.Listen)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("Получен сигнал остановки, завершаю работу")
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
