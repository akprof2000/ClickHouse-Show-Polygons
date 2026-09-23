package main

// Получение логина и пароля ClickHouse из PAM (Privileged Access Management)
// по протоколу AAPM. Схема повторяет проект mrr2h3: приложение приходит в PAM
// со своим AAPM-токеном, называет путь записи и получает актуальные учётные
// данные. Токен берётся только из переменной окружения — ни в конфиге, ни в
// браузере его нет.
//
// Ответ кэшируется на TTL, чтобы не ходить в PAM на каждый запрос и не
// засорять журнал аудита. Если ClickHouse отвечает «неверный пароль», кэш
// сбрасывается (Invalidate) и попытка повторяется один раз — так подхватывается
// плановая смена пароля.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/akprof2000/pam-client/pam"
)

// DefaultPAMTTL — сколько времени полученные из PAM данные считаются свежими.
const DefaultPAMTTL = 10 * time.Minute

// Enabled сообщает, задана ли запись PAM.
func (c PAMConfig) Enabled() bool { return strings.TrimSpace(c.Secret) != "" }

// Credentials — источник учётных данных ClickHouse.
type Credentials interface {
	Get(ctx context.Context) (user, password string, err error)
	Invalidate()
}

// staticCreds — логин из конфига, пароль из переменной окружения.
type staticCreds struct{ user, password string }

func (s staticCreds) Get(context.Context) (string, string, error) {
	if s.password == "" {
		return s.user, "", nil // ClickHouse без пароля — допустимо на стенде
	}
	return s.user, s.password, nil
}
func (staticCreds) Invalidate() {}

// pamCreds получает и кэширует учётные данные из PAM.
type pamCreds struct {
	client *pam.Client
	secret string
	ttl    time.Duration

	mu       sync.Mutex
	user     string
	password string
	fetched  time.Time
}

// newPAMCreds создаёт резолвер. Токен читается из окружения сразу, чтобы
// ошибка настройки всплыла при запуске, а не при первом запросе карты.
func newPAMCreds(c PAMConfig) (*pamCreds, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("pam: не задан путь записи (secret)")
	}
	env := c.TokenEnv
	if env == "" {
		env = "PAM_TOKEN"
	}
	token := os.Getenv(env)
	if token == "" {
		return nil, fmt.Errorf("pam: переменная окружения %s пуста или не задана", env)
	}
	opts := []pam.Option{pam.WithUserAgent("chviewer")}
	if c.Timeout > 0 {
		opts = append(opts, pam.WithTimeout(c.Timeout.D()))
	}
	if c.Comment != "" {
		opts = append(opts, pam.WithComment(c.Comment))
	}
	for _, ca := range c.CACert {
		opts = append(opts, pam.WithCACertFile(ca))
	}
	if (c.ClientCert == "") != (c.ClientKey == "") {
		return nil, fmt.Errorf("pam: для взаимного TLS нужны оба файла — сертификат и ключ")
	}
	if c.ClientCert != "" {
		opts = append(opts, pam.WithClientCert(c.ClientCert, c.ClientKey))
	}
	if c.Insecure {
		opts = append(opts, pam.WithInsecureSkipVerify(true))
	}
	cl, err := pam.New(c.Server, token, opts...)
	if err != nil {
		return nil, fmt.Errorf("pam: %w", err)
	}
	ttl := c.TTL.D()
	if ttl <= 0 {
		ttl = DefaultPAMTTL
	}
	return &pamCreds{client: cl, secret: c.Secret, ttl: ttl}, nil
}

func (p *pamCreds) Get(ctx context.Context) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.password != "" && time.Since(p.fetched) < p.ttl {
		return p.user, p.password, nil
	}
	s, err := p.client.Get(ctx, p.secret)
	if err != nil {
		return "", "", fmt.Errorf("pam: получение записи %s: %w", p.secret, err)
	}
	if s.Kind != pam.KindUserCredentials {
		return "", "", fmt.Errorf("pam: запись %s имеет тип %q, а нужен логин с паролем", p.secret, s.Kind)
	}
	if s.Username == "" || s.Password == "" {
		return "", "", fmt.Errorf("pam: в записи %s нет логина или пароля", p.secret)
	}
	p.user, p.password, p.fetched = s.Username, s.Password, time.Now()
	return p.user, p.password, nil
}

func (p *pamCreds) Invalidate() {
	p.mu.Lock()
	p.password, p.user, p.fetched = "", "", time.Time{}
	p.mu.Unlock()
}

// newCredentials выбирает источник по конфигурации.
func newCredentials(c CHConfig) (Credentials, error) {
	if c.PAM.Enabled() {
		return newPAMCreds(c.PAM)
	}
	pw := ""
	if c.PasswordEnv != "" {
		pw = os.Getenv(c.PasswordEnv)
		if pw == "" {
			return nil, fmt.Errorf("clickhouse: переменная окружения %s пуста или не задана", c.PasswordEnv)
		}
	}
	return staticCreds{user: c.User, password: pw}, nil
}
