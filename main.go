// Команда chviewer — веб-сервер, показывающий полигоны из ClickHouse на
// карте OpenStreetMap.
//
// Приложение работает как служба: пользователи открывают его в браузере,
// один раз вводят токен доступа и дальше работают со слоями. Адрес ClickHouse
// задаётся в конфигурации сервера, а логин с паролем берутся из PAM — в
// браузер они не попадают и нигде на стороне пользователя не хранятся.
// Настройки слоёв сохраняются на сервере как общие шаблоны.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed web/dist
var webFS embed.FS

// версия приложения; подставляется при сборке:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "dev"

func main() {
	var (
		cfgPath  = flag.String("config", "chviewer.yaml", "путь к файлу конфигурации")
		listen   = flag.String("listen", "", "переопределить адрес из конфигурации, например :8081")
		initCfg  = flag.Bool("init", false, "создать файл-пример конфигурации и выйти")
		showVer  = flag.Bool("version", false, "показать версию и выйти")
		checkCfg = flag.Bool("check", false, "проверить конфигурацию и доступность ClickHouse, затем выйти")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	if *initCfg {
		if err := WriteExample(*cfgPath); err != nil {
			log.Fatalf("FATAL %v", err)
		}
		fmt.Printf("создан %s — отредактируйте его и запустите: chviewer -config %s\n", *cfgPath, *cfgPath)
		return
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("FATAL конфигурация: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	log.Printf("=== ClickHouse Show Polygons v%s ===", version)
	log.Printf("конфигурация: %s | данные: %s", *cfgPath, cfg.DataDir)
	if cfg.AuthToken() == "" {
		log.Printf("ВНИМАНИЕ вход без токена: задайте auth.token или переменную %s", cfg.Auth.TokenEnv)
	}

	creds, err := newCredentials(cfg.ClickHouse)
	if err != nil {
		log.Fatalf("FATAL учётные данные ClickHouse: %v", err)
	}
	ch := newCHClient(cfg.ClickHouse, creds)

	// проверяем связку «конфиг + PAM + ClickHouse» на старте: понятная
	// ошибка в журнале службы лучше, чем пустая карта у пользователя
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	user, err := ch.Check(ctx)
	cancel()
	if err != nil {
		if *checkCfg {
			log.Fatalf("FATAL проверка не пройдена: %v", err)
		}
		log.Printf("ВНИМАНИЕ ClickHouse сейчас недоступен (%v) — сервер всё равно запускается", err)
	} else {
		log.Printf("ClickHouse %s доступен, пользователь %s (источник: %s)",
			cfg.ClickHouse.URL, user, credSource(cfg.ClickHouse))
	}
	if *checkCfg {
		fmt.Println("конфигурация в порядке")
		return
	}

	tpl, err := newTemplateStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("FATAL %v", err)
	}

	// Go на Windows берёт типы из реестра и отдаёт .mjs как text/plain, а
	// модульный воркер maplibre с таким типом не загрузится (тем более под
	// nosniff). Регистрируем правильный тип явно.
	if err := mime.AddExtensionType(".mjs", "text/javascript"); err != nil {
		log.Printf("WARN не удалось задать тип для .mjs: %v", err)
	}

	static, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Fatalf("FATAL встроенные файлы недоступны: %v", err)
	}

	srv := NewServer(cfg, ch, tpl, static, version)
	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(runCtx); err != nil {
		log.Fatalf("FATAL сервер остановился: %v", err)
	}
	log.Printf("сервер остановлен")
}
