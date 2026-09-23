package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Конфигурация сервера. Всё, что раньше пользователь вводил в браузере
// (адрес ClickHouse, логин, пароль), теперь задаётся здесь один раз
// администратором: браузер этих данных не видит вообще.

// Duration — time.Duration, который YAML умеет читать как "10s" или "10m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if strings.TrimSpace(s) == "" {
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("некорректная длительность %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

type Config struct {
	// Listen — адрес сервера, например ":8081" или "127.0.0.1:8081".
	Listen string `yaml:"listen"`
	// Title — заголовок страницы.
	Title string `yaml:"title"`
	// DataDir — где лежат общие шаблоны слоёв.
	DataDir string `yaml:"data_dir"`

	Auth       AuthConfig `yaml:"auth"`
	ClickHouse CHConfig   `yaml:"clickhouse"`
	TLS        TLSConfig  `yaml:"tls"`

	// Basemaps — подложки карты на выбор. Пусто = набор по умолчанию.
	Basemaps []Basemap `yaml:"basemaps"`
}

// Basemap — растровая подложка. Какие источники включать, решает
// администратор: у Яндекса, 2ГИС и Google правила использования требуют
// работать через их собственные API, поэтому по умолчанию они выключены.
type Basemap struct {
	Name        string   `yaml:"name" json:"name"`
	Tiles       []string `yaml:"tiles" json:"tiles"`
	Attribution string   `yaml:"attribution" json:"attribution"`
	MaxZoom     int      `yaml:"max_zoom" json:"max_zoom"`
	TileSize    int      `yaml:"tile_size" json:"tile_size"`
	// Projection: "3857" (по умолчанию) или "3395" — эллипсоидальный
	// меркатор, в котором отдаёт тайлы Яндекс. Подложка в 3395 смещается
	// относительно данных, поэтому режим помечается в интерфейсе.
	Projection string `yaml:"projection" json:"projection"`
}

// defaultBasemaps — то, что можно отдавать без отдельных договорённостей.
func defaultBasemaps() []Basemap {
	return []Basemap{
		{
			Name:        "OpenStreetMap",
			Tiles:       []string{"https://tile.openstreetmap.org/{z}/{x}/{y}.png"},
			Attribution: "© OpenStreetMap contributors",
			MaxZoom:     19,
		},
		{
			Name:        "OSM светлая (Carto)",
			Tiles:       []string{"https://a.basemaps.cartocdn.com/light_all/{z}/{x}/{y}.png"},
			Attribution: "© OpenStreetMap contributors, © CARTO",
			MaxZoom:     19,
		},
		{
			Name:        "OSM тёмная (Carto)",
			Tiles:       []string{"https://a.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}.png"},
			Attribution: "© OpenStreetMap contributors, © CARTO",
			MaxZoom:     19,
		},
		{
			Name:        "Спутник (Esri)",
			Tiles:       []string{"https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}"},
			Attribution: "© Esri, Maxar, Earthstar Geographics",
			MaxZoom:     19,
		},
		{
			Name:        "Рельеф (OpenTopoMap)",
			Tiles:       []string{"https://a.tile.opentopomap.org/{z}/{x}/{y}.png"},
			Attribution: "© OpenStreetMap contributors, SRTM, © OpenTopoMap (CC-BY-SA)",
			MaxZoom:     17,
		},
	}
}

// normalizeBasemaps подставляет значения по умолчанию и отбрасывает пустые.
func (c *Config) normalizeBasemaps() {
	if len(c.Basemaps) == 0 {
		c.Basemaps = defaultBasemaps()
	}
	out := c.Basemaps[:0]
	for _, b := range c.Basemaps {
		if strings.TrimSpace(b.Name) == "" || len(b.Tiles) == 0 {
			continue
		}
		if b.MaxZoom <= 0 {
			b.MaxZoom = 19
		}
		if b.TileSize <= 0 {
			b.TileSize = 256
		}
		if b.Projection == "" {
			b.Projection = "3857"
		}
		out = append(out, b)
	}
	c.Basemaps = out
}

// AuthConfig — вход в веб-интерфейс. Токен спрашивается у пользователя один
// раз и кладётся в cookie. Пустой токен = вход открыт (только для стенда).
type AuthConfig struct {
	Token    string `yaml:"token"`
	TokenEnv string `yaml:"token_env"`
}

// CHConfig — куда ходить за данными. Набор полей и их смысл такие же, как в
// проекте mrr2h3: сервер задаётся парами host:port, защита соединения —
// режимом tls, логин и пароль берутся из PAM (если задан pam.secret) либо из
// user + password_env.
type CHConfig struct {
	// Addr — host:port native-протокола ClickHouse (9000 без TLS, 9440 с TLS).
	// Можно несколько: драйвер сам выбирает живой узел.
	Addr     []string `yaml:"addr"`
	Database string   `yaml:"database"`

	User        string `yaml:"user"`
	PasswordEnv string `yaml:"password_env"`

	// TLS — off | on | ca | insecure (как в mrr2h3):
	//   off      — обычное соединение;
	//   on       — TLS с системными корневыми сертификатами;
	//   ca       — TLS с доверием сертификатам из ca_cert;
	//   insecure — TLS без проверки сертификата (только стенд).
	TLS     string   `yaml:"tls"`
	CACert  []string `yaml:"ca_cert"`
	TLSCert string   `yaml:"tls_cert"` // взаимный TLS: клиентский сертификат
	TLSKey  string   `yaml:"tls_key"`

	Timeout Duration  `yaml:"timeout"`
	PAM     PAMConfig `yaml:"pam"`
}

// TLSMode — режим защиты соединения с ClickHouse.
type TLSMode string

const (
	TLSOff      TLSMode = "off"
	TLSOn       TLSMode = "on"
	TLSCustomCA TLSMode = "ca"
	TLSInsecure TLSMode = "insecure"
)

// Mode возвращает режим TLS, подставляя off для пустого значения.
func (c CHConfig) Mode() TLSMode {
	if strings.TrimSpace(c.TLS) == "" {
		return TLSOff
	}
	return TLSMode(strings.ToLower(strings.TrimSpace(c.TLS)))
}

// defaultPort: native-протокол ClickHouse слушает 9000, с TLS — 9440.
// Порт, указанный в addr, всегда важнее.
func (c CHConfig) defaultPort() string {
	if c.Mode() == TLSOff {
		return "9000"
	}
	return "9440"
}

// AddrList приводит addr к виду host:port, подставляя порт по режиму TLS.
func (c CHConfig) AddrList() []string {
	out := make([]string, 0, len(c.Addr))
	for _, a := range c.Addr {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !strings.Contains(a, ":") {
			a += ":" + c.defaultPort()
		}
		out = append(out, a)
	}
	return out
}

// TLSConfig собирает настройку TLS для выбранного режима (nil без TLS).
// Логика повторяет mrr2h3, включая проверки на несочетаемые параметры.
func (c CHConfig) TLSConfig() (*tls.Config, error) {
	var cfg *tls.Config
	switch c.Mode() {
	case TLSOff:
		if c.TLSCert != "" || c.TLSKey != "" {
			return nil, fmt.Errorf("clickhouse: клиентский сертификат задан, но tls: off — выберите on|ca|insecure")
		}
		return nil, nil
	case TLSOn:
		cfg = &tls.Config{}
	case TLSInsecure:
		cfg = &tls.Config{InsecureSkipVerify: true}
	case TLSCustomCA:
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if len(c.CACert) == 0 {
			return nil, fmt.Errorf("clickhouse: режим tls: ca требует хотя бы один файл в ca_cert")
		}
		for _, f := range c.CACert {
			pem, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("clickhouse: чтение корневого сертификата: %w", err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("clickhouse: в %s нет сертификатов PEM", f)
			}
		}
		cfg = &tls.Config{RootCAs: pool}
	default:
		return nil, fmt.Errorf("clickhouse: неизвестный режим tls %q (нужен off|on|ca|insecure)", c.TLS)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("clickhouse: для взаимного TLS нужны оба файла — tls_cert и tls_key")
	}
	if c.TLSCert != "" {
		pair, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: загрузка клиентского сертификата: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// PAMConfig — запись в PAM (Privileged Access Management) с логином и
// паролем ClickHouse. Сам AAPM-токен в файле не хранится: только имя
// переменной окружения, откуда его взять.
type PAMConfig struct {
	Secret     string   `yaml:"secret"`
	Server     string   `yaml:"server"`
	TokenEnv   string   `yaml:"token_env"`
	Comment    string   `yaml:"comment"`
	CACert     []string `yaml:"ca_cert"`
	ClientCert string   `yaml:"tls_cert"`
	ClientKey  string   `yaml:"tls_key"`
	Insecure   bool     `yaml:"insecure"`
	Timeout    Duration `yaml:"timeout"`
	TTL        Duration `yaml:"ttl"`
}

// TLSConfig — HTTPS самого веб-сервера (браузер → chviewer). Пусто = HTTP,
// это нормально за nginx или в доверенной сети.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

func defaultConfig() Config {
	return Config{
		Listen:  ":8081",
		Title:   "ClickHouse Show Polygons",
		DataDir: "./data",
		Auth:    AuthConfig{TokenEnv: "CHVIEWER_TOKEN"},
		ClickHouse: CHConfig{
			Addr:     []string{"localhost:9000"},
			Database: "default",
			TLS:      string(TLSOff),
			Timeout:  Duration(120 * time.Second),
			PAM:      PAMConfig{TokenEnv: "PAM_TOKEN", Comment: "chviewer", Timeout: Duration(10 * time.Second)},
		},
	}
}

// LoadConfig читает YAML поверх значений по умолчанию и проверяет его.
func LoadConfig(path string) (Config, error) {
	c := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("разбор %s: %w", path, err)
	}
	if err := c.normalize(); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) normalize() error {
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = ":8081"
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = "./data"
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return fmt.Errorf("data_dir: %w", err)
	}
	c.DataDir = abs

	if len(c.ClickHouse.AddrList()) == 0 {
		return fmt.Errorf("clickhouse.addr: укажите хотя бы один адрес host:port")
	}
	if _, err := c.ClickHouse.TLSConfig(); err != nil {
		return err
	}
	if c.ClickHouse.Timeout <= 0 {
		c.ClickHouse.Timeout = Duration(120 * time.Second)
	}
	// Два способа получить пароль исключают друг друга: иначе непонятно,
	// какой из них действует, и легко оставить в конфиге мёртвую настройку.
	if c.ClickHouse.PAM.Secret != "" && c.ClickHouse.PasswordEnv != "" {
		return fmt.Errorf("clickhouse: password_env и pam.secret взаимоисключающи")
	}
	if c.ClickHouse.PAM.Secret == "" && c.ClickHouse.PasswordEnv == "" && c.ClickHouse.User == "" {
		return fmt.Errorf("clickhouse: задайте либо pam.secret, либо user + password_env")
	}
	c.normalizeBasemaps()
	if len(c.Basemaps) == 0 {
		return fmt.Errorf("basemaps: ни одной подложки — у каждой нужны name и tiles")
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return fmt.Errorf("tls: нужны оба файла — cert_file и key_file")
	}
	return nil
}

// AuthToken — действующий токен доступа ("" = вход открыт).
func (c Config) AuthToken() string {
	if c.Auth.Token != "" {
		return c.Auth.Token
	}
	if c.Auth.TokenEnv != "" {
		return os.Getenv(c.Auth.TokenEnv)
	}
	return ""
}

const exampleConfig = `# Конфигурация ClickHouse Show Polygons (серверный режим).
# Запуск: chviewer -config /etc/chviewer.yaml

listen: ":8081"                 # адрес сервера; "127.0.0.1:8081" — только локально
title: "ClickHouse Show Polygons"
data_dir: "/var/lib/chviewer"   # здесь хранятся общие шаблоны слоёв

# Вход в веб-интерфейс. Пользователь вводит токен один раз, дальше cookie.
# Пустой токен = вход без пароля (только для закрытого стенда).
auth:
  token: ""                     # можно задать прямо здесь
  token_env: "CHVIEWER_TOKEN"   # но лучше через переменную окружения

# Куда ходить за полигонами. Браузер этих данных не получает.
# Набор настроек такой же, как в проекте mrr2h3.
clickhouse:
  addr: ["clickhouse.example.com:9000"]  # host:port native-протокола (9440 при TLS);
                                         # можно несколько — драйвер выберет живой
  database: "default"     # база по умолчанию: слои можно писать без префикса "база."
  timeout: 120s

  # Первый способ входа: логин здесь, пароль в переменной окружения.
  user: ""
  password_env: ""

  # Второй способ: логин и пароль берутся из PAM (AAPM) по пути записи.
  # Задан secret => user/password_env не используются. Сам токен PAM
  # берётся только из переменной окружения, в этом файле его нет.
  pam:
    secret: ""                  # "/Группа/Подгруппа/запись"; пусто = PAM не используется
    server: ""                  # https://pam.example.com (пусто = переменная PAM_SERVER)
    token_env: "PAM_TOKEN"      # имя переменной окружения с AAPM-токеном
    comment: "chviewer"         # комментарий в журнал аудита PAM
    ca_cert: []                 # PEM корневых сертификатов PAM
    tls_cert: ""                # клиентский сертификат для PAM (взаимный TLS)
    tls_key: ""
    insecure: false             # true = не проверять сертификат PAM (только стенд)
    timeout: 10s                # таймаут одного запроса к PAM
    ttl: 10m                    # сколько держать полученный пароль в памяти

  # Защита соединения с ClickHouse:
  #   off      — обычное соединение (порт 9000 по умолчанию);
  #   on       — TLS с системными корневыми сертификатами (9440);
  #   ca       — TLS с доверием сертификатам из ca_cert;
  #   insecure — TLS без проверки сертификата (только стенд).
  tls: "off"
  ca_cert: []             # ["/etc/ssl/ch-ca.pem"] для tls: ca
  tls_cert: ""            # клиентский сертификат (взаимный TLS)
  tls_key: ""             # ключ клиентского сертификата

# Подложки карты на выбор. Пусто = встроенный набор: OpenStreetMap, светлая и
# тёмная Carto, спутник Esri, рельеф OpenTopoMap.
#
# Яндекс, 2ГИС и Google по своим правилам разрешают тайлы только через их
# собственные API и SDK, поэтому здесь их нет. Если у вас есть договор или
# внутренний прокси, добавьте их сами — примеры закомментированы.
basemaps: []
#  - name: "OpenStreetMap"
#    tiles: ["https://tile.openstreetmap.org/{z}/{x}/{y}.png"]
#    attribution: "© OpenStreetMap contributors"
#    max_zoom: 19
#  - name: "Яндекс"
#    tiles: ["https://core-renderer-tiles.maps.yandex.net/tiles?l=map&x={x}&y={y}&z={z}&scale=1&lang=ru_RU"]
#    attribution: "© Яндекс"
#    projection: "3395"   # Яндекс отдаёт эллипсоидальный меркатор: подложка
#                         # смещается относительно данных, тем сильнее, чем
#                         # дальше от экватора
#  - name: "2ГИС"
#    tiles: ["https://tile2.maps.2gis.com/tiles?x={x}&y={y}&z={z}"]
#    attribution: "© 2ГИС"
#  - name: "Google"
#    tiles: ["https://mt0.google.com/vt/lyrs=m&x={x}&y={y}&z={z}"]
#    attribution: "© Google"
#  - name: "Свой тайл-сервер"
#    tiles: ["https://tiles.corp.local/{z}/{x}/{y}.png"]
#    attribution: "Внутренний тайл-сервер"

# HTTPS самого веб-сервера. Пусто = HTTP (нормально за nginx).
tls:
  cert_file: ""
  key_file: ""
`

// WriteExample создаёт файл-пример конфигурации.
func WriteExample(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s уже существует — удалите или укажите другой путь", path)
	}
	return os.WriteFile(path, []byte(exampleConfig), 0o600)
}
