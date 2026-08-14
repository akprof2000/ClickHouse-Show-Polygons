package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"embed"
	"io/fs"
)

//go:embed web/dist
var webFS embed.FS

// версия приложения; подставляется при сборке:
//   go build -ldflags "-X main.version=1.2.3"
var version = "dev"

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func configPath() string { return filepath.Join(exeDir(), "config.json") }

// ---------- логирование ----------

func setupLog() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

// первые n символов SQL одной строкой, без переносов
func trimSQL(sql string, n int) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---------- конфиг ----------

func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(configPath())
		if err != nil {
			log.Printf("CONFIG чтение: файла %s ещё нет (%v) — отдаю пустой конфиг", configPath(), err)
			w.Write([]byte("{}"))
			return
		}
		log.Printf("CONFIG чтение: отдано %d байт из %s", len(data), configPath())
		w.Write(data)
	case http.MethodPost:
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || !json.Valid(data) {
			log.Printf("CONFIG сохранение: пришёл некорректный JSON (err=%v, %d байт)", err, len(data))
			http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(configPath(), data, 0600); err != nil {
			log.Printf("ERROR CONFIG сохранение в %s: %v", configPath(), err)
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
			return
		}
		log.Printf("CONFIG сохранение: записано %d байт в %s", len(data), configPath())
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": configPath()})
	default:
		http.Error(w, `{"error":"method"}`, http.StatusMethodNotAllowed)
	}
}

// ---------- прокси-запрос к ClickHouse ----------

type queryReq struct {
	URL      string `json:"url"`      // http://host:8123/ или https://host:8443/
	User     string `json:"user"`
	Password string `json:"password"` // расшифрованный браузером, только на время запроса; в лог не пишется
	Insecure bool   `json:"insecure"` // не проверять сертификат
	SQL      string `json:"sql"`
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method"}`, http.StatusMethodNotAllowed)
		return
	}
	var q queryReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil {
		log.Printf("QUERY отклонён: некорректный JSON в запросе от браузера: %v", err)
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	u, err := url.Parse(q.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		log.Printf("QUERY отклонён: некорректный адрес ClickHouse %q (err=%v)", q.URL, err)
		http.Error(w, `{"error":"bad clickhouse url"}`, http.StatusBadRequest)
		return
	}

	log.Printf("QUERY -> %s | user=%s | ssl=%v insecure=%v | полный SQL:\n%s",
		u.String(), q.User, u.Scheme == "https", q.Insecure, q.SQL)
	start := time.Now()

	client := &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: q.Insecure},
		},
	}
	req, _ := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(q.SQL))
	req.Header.Set("X-ClickHouse-User", q.User)
	req.Header.Set("X-ClickHouse-Key", q.Password)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("ERROR QUERY %s: соединение не удалось за %s: %v", u.Host, time.Since(start).Round(time.Millisecond), err)
		writeErr(w, "Соединение не удалось: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	dur := time.Since(start).Round(time.Millisecond)
	if readErr != nil {
		log.Printf("ERROR QUERY %s: тело ответа оборвалось после %d байт за %s: %v", u.Host, len(body), dur, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("ERROR QUERY %s: HTTP %d за %s, ответ сервера: %s", u.Host, resp.StatusCode, dur, trimSQL(string(body), 500))
		writeErr(w, string(body))
		return
	}
	log.Printf("QUERY OK %s: HTTP 200, %d байт за %s", u.Host, len(body), dur)
	w.Write(body)
}

// ---------- кэш объектов на стороне бэка ----------

type rect struct{ W, S, E, N float64 }

func (r rect) contains(o rect) bool { return o.W >= r.W && o.E <= r.E && o.S >= r.S && o.N <= r.N }
func (r rect) intersects(o rect) bool {
	return r.W <= o.E && r.E >= o.W && r.S <= o.N && r.N >= o.S
}

type cachedObj struct {
	bb  rect
	row map[string]any
}

// покрытая область помнит, с какой детализацией её грузили: minDeg —
// минимальный размер объекта (в градусах), который тогда попадал в выборку.
// Для более детального запроса (меньший minDeg) эта область покрытием не считается.
type coveredRect struct {
	rect
	minDeg float64
}

type layerCache struct {
	objs  map[string]cachedObj
	rects []coveredRect // полностью покрытые области
}

// кэш раздельный по слоям: ключ url|user|table|geo
var (
	cacheMu sync.Mutex
	caches  = map[string]*layerCache{}
)

type objectsReq struct {
	URL      string    `json:"url"`
	User     string    `json:"user"`
	Password string    `json:"password"`
	Insecure bool      `json:"insecure"`
	Table    string    `json:"table"`
	Geo      string    `json:"geo"`
	Bbox     []float64 `json:"bbox"`    // [w,s,e,n]
	Limit    int       `json:"limit"`
	MinDeg   float64   `json:"min_deg"` // фильтр детализации: объекты мельче этого размера (в градусах) не грузятся
	Reset    bool      `json:"reset"`   // принудительно сбросить кэш (кнопка «Подключиться»)
}

// bbox мультиполигона из координат [[[[x,y],...]]]
func geomBBox(g any) (rect, bool) {
	bb := rect{W: 1e18, S: 1e18, E: -1e18, N: -1e18}
	found := false
	var walk func(v any)
	walk = func(v any) {
		arr, ok := v.([]any)
		if !ok {
			return
		}
		// точка: [x, y]
		if len(arr) == 2 {
			x, xo := arr[0].(float64)
			y, yo := arr[1].(float64)
			if xo && yo {
				if x < bb.W {
					bb.W = x
				}
				if x > bb.E {
					bb.E = x
				}
				if y < bb.S {
					bb.S = y
				}
				if y > bb.N {
					bb.N = y
				}
				found = true
				return
			}
		}
		for _, e := range arr {
			walk(e)
		}
	}
	walk(g)
	return bb, found
}

func sqlIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "") + "`" }

func handleObjects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method"}`, http.StatusMethodNotAllowed)
		return
	}
	var q objectsReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil || len(q.Bbox) != 4 {
		log.Printf("OBJECTS отклонён: некорректный запрос от браузера (err=%v)", err)
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if q.Limit <= 0 {
		q.Limit = 3000
	}
	view := rect{W: q.Bbox[0], S: q.Bbox[1], E: q.Bbox[2], N: q.Bbox[3]}

	cacheMu.Lock()
	defer cacheMu.Unlock()

	key := q.URL + "|" + q.User + "|" + q.Table + "|" + q.Geo
	cache, ok := caches[key]
	if !ok || q.Reset {
		if ok {
			log.Printf("CACHE сброс слоя %q; было объектов: %d", key, len(cache.objs))
		}
		cache = &layerCache{objs: map[string]cachedObj{}}
		caches[key] = cache
	}

	// экран уже полностью внутри области, покрытой с достаточной детализацией — БД не трогаем
	fromCacheOnly := false
	for _, c := range cache.rects {
		if c.contains(view) && c.minDeg <= q.MinDeg*1.0001 {
			fromCacheOnly = true
			break
		}
	}

	usedSQL := ""
	if !fromCacheOnly {
		// запрашиваем с запасом +30%, чтобы мелкие сдвиги не порождали запросов
		padX, padY := (view.E-view.W)*0.3, (view.N-view.S)*0.3
		req := rect{W: view.W - padX, S: view.S - padY, E: view.E + padX, N: view.N + padY}

		geo, table := sqlIdent(q.Geo), q.Table
		// исключаем территории, уже покрытые с достаточной детализацией: объект,
		// пересекающий такую область, гарантированно есть в кэше — заново он не нужен
		skip := ""
		for _, c := range cache.rects {
			if c.minDeg > q.MinDeg*1.0001 {
				continue // область грузилась грубее — мелкие объекты оттуда ещё не читались
			}
			skip += fmt.Sprintf("\n  AND NOT (arrayMin(xs) <= %g AND arrayMax(xs) >= %g AND arrayMin(ys) <= %g AND arrayMax(ys) >= %g)",
				c.E, c.W, c.N, c.S)
		}
		// фильтр детализации: слишком мелкие для текущего масштаба объекты не читаем
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
LIMIT %d
FORMAT JSON`, geo, geo, table, req.E, req.W, req.N, req.S, skip, q.Limit)

		log.Printf("OBJECTS -> %s | кэш: %d объектов, %d областей | полный SQL:\n%s",
			q.URL, len(cache.objs), len(cache.rects), usedSQL)

		body, err := chDo(q.URL, q.User, q.Password, q.Insecure, usedSQL)
		if err != nil {
			writeErrSQL(w, err.Error(), usedSQL)
			return
		}
		var parsed struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			writeErrSQL(w, "не удалось разобрать ответ ClickHouse: "+err.Error(), usedSQL)
			return
		}
		added := 0
		for _, row := range parsed.Data {
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
		// область считаем покрытой, только если лимит не отрезал хвост;
		// вложенные в уже покрытые не добавляем, список ограничиваем
		if len(parsed.Data) < q.Limit {
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

	// отдаём из кэша всё, что пересекает экран и достаточно крупно для него
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
	json.NewEncoder(w).Encode(map[string]any{
		"rows": len(out), "cached": fromCacheOnly, "sql": usedSQL,
		"total_cached": len(cache.objs), "data": out,
	})
}

// ---------- контекстный поиск по всем полям таблицы ----------

type searchReq struct {
	URL      string `json:"url"`
	User     string `json:"user"`
	Password string `json:"password"`
	Insecure bool   `json:"insecure"`
	Table    string `json:"table"`
	Geo      string `json:"geo"`
	Query    string `json:"query"`
	Limit    int    `json:"limit"`
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method"}`, http.StatusMethodNotAllowed)
		return
	}
	var q searchReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil || strings.TrimSpace(q.Query) == "" {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}

	// структура таблицы: ищем по всем колонкам, кроме геометрии
	descBody, err := chDo(q.URL, q.User, q.Password, q.Insecure, "DESCRIBE TABLE "+q.Table+" FORMAT JSON")
	if err != nil {
		writeErrSQL(w, err.Error(), "DESCRIBE TABLE "+q.Table)
		return
	}
	var desc struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(descBody, &desc); err != nil {
		writeErrSQL(w, "не удалось разобрать DESCRIBE: "+err.Error(), "")
		return
	}
	var cols, strs []string
	for _, c := range desc.Data {
		if c.Name == q.Geo {
			continue
		}
		cols = append(cols, sqlIdent(c.Name))
		strs = append(strs, "toString("+sqlIdent(c.Name)+")")
	}
	if len(cols) == 0 {
		writeErrSQL(w, "в таблице нет колонок для поиска (кроме геометрии)", "")
		return
	}

	// экранируем строку поиска для SQL-литерала
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
LIMIT %d
FORMAT JSON`,
		geo, strings.Join(cols, ", "), geo, q.Table, strings.Join(strs, ", "), esc, q.Limit)

	log.Printf("SEARCH -> %s | таблица %s | запрос %q", q.URL, q.Table, q.Query)
	body, err := chDo(q.URL, q.User, q.Password, q.Insecure, sql)
	if err != nil {
		writeErrSQL(w, err.Error(), sql)
		return
	}
	var parsed struct {
		Data []map[string]any `json:"data"`
		Rows int              `json:"rows"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeErrSQL(w, "не удалось разобрать ответ ClickHouse: "+err.Error(), sql)
		return
	}
	log.Printf("SEARCH OK: %d совпадений в %s", len(parsed.Data), q.Table)
	json.NewEncoder(w).Encode(map[string]any{"rows": len(parsed.Data), "sql": sql, "data": parsed.Data})
}

// общий помощник: выполнить SQL на ClickHouse
func chDo(chURL, user, password string, insecure bool, sql string) ([]byte, error) {
	u, err := url.Parse(chURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("некорректный адрес ClickHouse: %q", chURL)
	}
	client := &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
		},
	}
	req, _ := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(sql))
	req.Header.Set("X-ClickHouse-User", user)
	req.Header.Set("X-ClickHouse-Key", password)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", string(body))
	}
	return body, nil
}

func writeErrSQL(w http.ResponseWriter, msg, sql string) {
	w.WriteHeader(http.StatusBadGateway)
	json.NewEncoder(w).Encode(map[string]string{"error": msg, "sql": sql})
}

func writeErr(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusBadGateway)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func openBrowser(url string) {
	if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start(); err != nil {
		log.Printf("WARN не удалось открыть браузер автоматически: %v — откройте %s вручную", err, url)
	} else {
		log.Printf("Браузер открыт: %s", url)
	}
}

func main() {
	setupLog()
	log.Printf("=== ClickHouse Viewer v%s запускается === exe: %s", version, exeDir())

	static, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Fatalf("FATAL встроенные файлы недоступны: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/query", handleQuery)
	mux.HandleFunc("/api/objects", handleObjects)
	mux.HandleFunc("/api/search", handleSearch)
	// машинный секрет для шифрования пароля: случайный, создаётся при первом
	// запуске, живёт рядом с exe. Ключ = PBKDF2(отпечаток браузера + секрет),
	// так что одного знания схемы для расшифровки недостаточно.
	mux.HandleFunc("/api/secret", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := filepath.Join(exeDir(), "secret.key")
		data, err := os.ReadFile(p)
		if err != nil || len(data) < 32 {
			buf := make([]byte, 32)
			if _, err := rand.Read(buf); err != nil {
				http.Error(w, `{"error":"rand"}`, http.StatusInternalServerError)
				return
			}
			data = []byte(hex.EncodeToString(buf))
			if err := os.WriteFile(p, data, 0600); err != nil {
				log.Printf("WARN не удалось сохранить secret.key: %v", err)
			} else {
				log.Printf("SECRET создан новый машинный секрет: %s", p)
			}
		}
		json.NewEncoder(w).Encode(map[string]string{"secret": string(data)})
	})
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": version})
	})

	// слушаем только localhost: порт 8137, если занят — какой даст система
	ln, err := net.Listen("tcp", "127.0.0.1:8137")
	if err != nil {
		log.Printf("WARN порт 8137 занят (%v), беру свободный", err)
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Fatalf("FATAL не удалось открыть порт: %v", err)
		}
	}
	addr := fmt.Sprintf("http://%s/", ln.Addr().String())
	log.Printf("Сервер слушает %s", addr)
	noBrowser := false
	for _, a := range os.Args[1:] {
		if a == "-nobrowser" {
			noBrowser = true
		}
	}
	if noBrowser {
		log.Printf("Флаг -nobrowser: браузер не открываю, откройте %s вручную", addr)
	} else {
		openBrowser(addr)
	}
	if err := http.Serve(ln, mux); err != nil {
		log.Fatalf("FATAL сервер остановился: %v", err)
	}
}
