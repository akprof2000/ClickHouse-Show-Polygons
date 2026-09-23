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
	// Addr — host:port HTTP-интерфейса ClickHouse (8123 без TLS, 8443 с TLS).
	// Можно несколько: сервер пробует их по очереди, пока кто-то не ответит.
	Addr     []string `yaml:"addr"`
	Database string   `yaml:"database"`

	User        string `yaml:"user"`
	PasswordEnv string `yaml:"password_env"`

	// TLS — off | on | ca | insecure (как в mrr2h3):
	//   off      — обычный HTTP;
	//   on       — HTTPS с системными корневыми сертификатами;
	//   ca       — HTTPS с доверием сертификатам из ca_cert;
	//   insecure — HTTPS без проверки сертификата (только стенд).
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

// Scheme и DefaultPort: по HTTP-интерфейсу ClickHouse слушает 8123, по
// HTTPS — 8443. Порт из addr, если он там указан, всегда важнее.
func (c CHConfig) Scheme() string {
	if c.Mode() == TLSOff {
		return "http"
	}
	return "https"
}

func (c CHConfig) defaultPort() string {
	if c.Mode() == TLSOff {
		return "8123"
	}
	return "8443"
}

// Endpoints превращает addr в полные адреса запросов.
func (c CHConfig) Endpoints() []string {
	out := make([]string, 0, len(c.Addr))
	for _, a := range c.Addr {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !strings.Contains(a, ":") {
			a += ":" + c.defaultPort()
		}
		out = append(out, c.Scheme()+"://"+a+"/")
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
			Addr:     []string{"localhost:8123"},
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

	if len(c.ClickHouse.Endpoints()) == 0 {
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
  addr: ["clickhouse.example.com:8123"]  # host:port HTTP-интерфейса (8443 при TLS);
                                         # можно несколько — пробуются по очереди
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
  #   off      — обычный HTTP (порт 8123 по умолчанию);
  #   on       — HTTPS с системными корневыми сертификатами (8443);
  #   ca       — HTTPS с доверием сертификатам из ca_cert;
  #   insecure — HTTPS без проверки сертификата (только стенд).
  tls: "off"
  ca_cert: []             # ["/etc/ssl/ch-ca.pem"] для tls: ca
  tls_cert: ""            # клиентский сертификат (взаимный TLS)
  tls_key: ""             # ключ клиентского сертификата

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
