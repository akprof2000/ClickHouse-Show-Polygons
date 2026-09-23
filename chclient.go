package main

// Клиент ClickHouse. Адрес берётся из конфига сервера, логин и пароль — у
// источника учётных данных (PAM или окружение). Браузер в этом не участвует.

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxResponse — предел ответа ClickHouse, который мы готовы прочитать.
const maxResponse = 200 << 20

type chClient struct {
	url     string
	creds   Credentials
	http    *http.Client
	timeout time.Duration
}

func newCHClient(c CHConfig, creds Credentials) *chClient {
	return &chClient{
		url:   strings.TrimSpace(c.URL),
		creds: creds,
		http: &http.Client{
			Timeout:   c.Timeout.D(),
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: c.Insecure}},
		},
		timeout: c.Timeout.D(),
	}
}

// Do выполняет SQL. Если ClickHouse отверг пароль, учётные данные считаются
// устаревшими (в PAM сменили пароль): сбрасываем кэш и пробуем ещё раз.
func (c *chClient) Do(ctx context.Context, sql string) ([]byte, error) {
	body, status, err := c.once(ctx, sql)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		c.creds.Invalidate()
		body, status, err = c.once(ctx, sql)
		if err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s", string(body))
	}
	return body, nil
}

func (c *chClient) once(ctx context.Context, sql string) ([]byte, int, error) {
	user, password, err := c.creds.Get(ctx)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(sql))
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
