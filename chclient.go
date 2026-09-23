package main

// Клиент ClickHouse на официальном драйвере clickhouse-go (native-протокол,
// порт 9000, при TLS 9440) — тот же, что в проекте mrr2h3. Адрес, режим TLS и
// сертификаты берутся из конфигурации сервера, логин и пароль — у источника
// учётных данных (PAM или окружение). Браузер в этом не участвует.
//
// Соединение живёт вместе с учётными данными: пароль в драйвере задаётся при
// открытии, поэтому при смене пароля в PAM соединение переоткрывается. Ошибки
// аутентификации ClickHouse (192, 193, 516) распознаются отдельно: кэш PAM
// сбрасывается и запрос повторяется один раз.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type chClient struct {
	addr     []string
	database string
	tls      *tls.Config
	creds    Credentials
	timeout  time.Duration

	mu       sync.Mutex
	conn     driver.Conn
	connUser string // под каким логином открыто текущее соединение
	connPass string
}

func newCHClient(c CHConfig, creds Credentials) (*chClient, error) {
	cfg, err := c.TLSConfig()
	if err != nil {
		return nil, err
	}
	return &chClient{
		addr:     c.AddrList(),
		database: strings.TrimSpace(c.Database),
		tls:      cfg,
		creds:    creds,
		timeout:  c.Timeout.D(),
	}, nil
}

// Addrs — адреса для журнала и страницы сведений.
func (c *chClient) Addrs() string { return strings.Join(c.addr, ", ") }

// Close закрывает соединение при остановке службы.
func (c *chClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// connect возвращает соединение, открывая новое, если учётные данные
// изменились (в PAM сменили пароль) или его ещё нет.
func (c *chClient) connect(ctx context.Context) (driver.Conn, error) {
	user, password, err := c.creds.Get(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.connUser == user && c.connPass == password {
		return c.conn, nil
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: c.addr,
		Auth: clickhouse.Auth{Database: c.database, Username: user, Password: password},
		TLS:  c.tls,
		// пул небольшой: запросы короткие, а карта дёргает сервер пачками
		MaxOpenConns: 8,
		MaxIdleConns: 4,
		DialTimeout:  10 * time.Second,
		ReadTimeout:  c.timeout,
		Compression:  &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		ClientInfo:   clickhouse.ClientInfo{Products: []struct{ Name, Version string }{{Name: "chviewer", Version: version}}},
	})
	if err != nil {
		return nil, fmt.Errorf("подключение к ClickHouse: %w", err)
	}
	c.conn, c.connUser, c.connPass = conn, user, password
	return conn, nil
}

// authFailed сообщает, что сервер отверг учётные данные: UNKNOWN_USER (192),
// WRONG_PASSWORD (193), AUTHENTICATION_FAILED (516).
func authFailed(err error) bool {
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		switch ex.Code {
		case 192, 193, 516:
			return true
		}
	}
	return false
}

// Query выполняет SELECT и отдаёт строки в виде, пригодном для JSON.
func (c *chClient) Query(ctx context.Context, sql string) ([]map[string]any, error) {
	rows, err := c.query(ctx, sql)
	if err != nil && authFailed(err) {
		// пароль сменили после выдачи: сбрасываем кэш и пробуем ещё раз
		c.creds.Invalidate()
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()
		rows, err = c.query(ctx, sql)
	}
	return rows, err
}

func (c *chClient) query(ctx context.Context, sql string) ([]map[string]any, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := rows.Columns()
	types := rows.ColumnTypes()
	out := make([]map[string]any, 0, 256)
	for rows.Next() {
		// приёмники создаём по типам колонок: запрос произвольный, заранее
		// структуру строки мы не знаем
		dest := make([]any, len(cols))
		for i, t := range types {
			dest[i] = reflectNew(t.ScanType())
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("чтение строки: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, name := range cols {
			row[name] = jsonValue(derefAny(dest[i]))
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Check проверяет связку «конфиг + учётные данные» при запуске: понятная
// ошибка в журнале службы лучше, чем пустая карта у пользователя.
func (c *chClient) Check(ctx context.Context) (string, error) {
	user, _, err := c.creds.Get(ctx)
	if err != nil {
		return "", err
	}
	if _, err := c.Query(ctx, "SELECT 1"); err != nil {
		return user, err
	}
	return user, nil
}
