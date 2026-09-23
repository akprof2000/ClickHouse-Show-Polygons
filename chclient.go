package main

// Клиент ClickHouse. Адрес, режим TLS и сертификаты берутся из конфигурации
// сервера, логин и пароль — у источника учётных данных (PAM или окружение).
// Браузер в этом не участвует.

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// maxResponse — предел ответа ClickHouse, который мы готовы прочитать.
const maxResponse = 200 << 20

type chClient struct {
	endpoints []string
	database  string
	creds     Credentials
	http      *http.Client

	mu   sync.Mutex
	next int // с какого адреса начинать: последний удачный
}

func newCHClient(c CHConfig, creds Credentials) (*chClient, error) {
	tlsCfg, err := c.TLSConfig()
	if err != nil {
		return nil, err
	}
	return &chClient{
		endpoints: c.Endpoints(),
		database:  strings.TrimSpace(c.Database),
		creds:     creds,
		http: &http.Client{
			Timeout:   c.Timeout.D(),
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// Addrs — адреса для журнала и страницы сведений.
func (c *chClient) Addrs() string { return strings.Join(c.endpoints, ", ") }

// Do выполняет SQL. Адреса перебираются по очереди, начиная с последнего
// удачного: недоступный узел не должен ронять запрос, если есть живой.
// Если ClickHouse отверг пароль, учётные данные считаются устаревшими
// (в PAM сменили пароль): сбрасываем кэш и пробуем ещё раз.
func (c *chClient) Do(ctx context.Context, sql string) ([]byte, error) {
	body, status, err := c.try(ctx, sql)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.creds.Invalidate()
		body, status, err = c.try(ctx, sql)
		if err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s", string(body))
	}
	return body, nil
}

// try обходит адреса, пока один не ответит хоть чем-то по сети.
func (c *chClient) try(ctx context.Context, sql string) ([]byte, int, error) {
	c.mu.Lock()
	start := c.next
	c.mu.Unlock()

	var lastErr error
	for i := range c.endpoints {
		idx := (start + i) % len(c.endpoints)
		body, status, err := c.once(ctx, c.endpoints[idx], sql)
		if err == nil {
			c.mu.Lock()
			c.next = idx
			c.mu.Unlock()
			return body, status, nil
		}
		lastErr = err
		if len(c.endpoints) > 1 {
			log.Printf("WARN ClickHouse %s недоступен: %v", c.endpoints[idx], err)
		}
	}
	return nil, 0, lastErr
}

func (c *chClient) once(ctx context.Context, endpoint, sql string) ([]byte, int, error) {
	user, password, err := c.creds.Get(ctx)
	if err != nil {
		return nil, 0, err
	}
	u := endpoint
	if c.database != "" {
		// база по умолчанию: слои можно писать без префикса "база."
		q := url.Values{"database": {c.database}}
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(sql))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-ClickHouse-User", user)
	req.Header.Set("X-ClickHouse-Key", password)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("соединение с ClickHouse не удалось: %w", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("ответ ClickHouse оборвался после %d байт: %w", len(body), readErr)
	}
	return body, resp.StatusCode, nil
}

// Check проверяет связку «конфиг + учётные данные» при запуске: понятная
// ошибка в журнале службы лучше, чем пустая карта у пользователя.
func (c *chClient) Check(ctx context.Context) (string, error) {
	user, _, err := c.creds.Get(ctx)
	if err != nil {
		return "", err
	}
	if _, err := c.Do(ctx, "SELECT 1 FORMAT JSON"); err != nil {
		return user, err
	}
	return user, nil
}
