// Команда reviewer — микросервис автоматического ревью задач ClickUp
// силами claude -p. См. README.md для деталей эксплуатации.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Zadnepr/go_clickup_claude/internal/clickup"
	"github.com/Zadnepr/go_clickup_claude/internal/config"
	"github.com/Zadnepr/go_clickup_claude/internal/httpapi"
	"github.com/Zadnepr/go_clickup_claude/internal/queue"
	"github.com/Zadnepr/go_clickup_claude/internal/review"
	"github.com/Zadnepr/go_clickup_claude/internal/slack"
	"github.com/Zadnepr/go_clickup_claude/internal/store"
)

// registeredWebhookEvents — события, на которые сервис подписывается при
// регистрации вебхука (см. Требование 1.1).
var registeredWebhookEvents = []string{
	"taskStatusUpdated",
	"taskTagUpdated",
	"taskCreated",
	"taskMoved",
}

func main() {
	registerWebhook := flag.Bool("register-webhook", false, "создать вебхук ClickUp, напечатать секрет и завершить работу")
	webhookURL := flag.String("webhook-url", "", "публичный URL для --register-webhook, например https://host/webhook/clickup")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(nil)
	if err != nil {
		logger.Error("invalid configuration", "error", err.Error())
		os.Exit(1)
	}

	if *registerWebhook {
		if err := runRegisterWebhook(cfg, *webhookURL, logger); err != nil {
			logger.Error("failed to register webhook", "error", err.Error())
			os.Exit(1)
		}
		return
	}

	if err := run(cfg, logger); err != nil {
		logger.Error("service exited with error", "error", err.Error())
		os.Exit(1)
	}
}

func runRegisterWebhook(cfg *config.Config, webhookURL string, logger *slog.Logger) error {
	if webhookURL == "" {
		return fmt.Errorf("--webhook-url обязателен для --register-webhook")
	}

	cuClient := clickup.NewClient(cfg.CUAPIToken, cfg.CUTeamID, clickup.WithLogger(logger))
	defer cuClient.Close()

	id, secret, err := cuClient.CreateWebhook(context.Background(), webhookURL, registeredWebhookEvents)
	if err != nil {
		return err
	}

	fmt.Printf("Вебхук создан: id=%s\nCU_WEBHOOK_SECRET=%s\n", id, secret)
	fmt.Println("Положите CU_WEBHOOK_SECRET в .env — без неё эндпоинт вебхука не поднимется.")
	return nil
}

func run(cfg *config.Config, logger *slog.Logger) error {
	if cfg.RepoURL != "" {
		if err := cloneRepoIfMissing(cfg, logger); err != nil {
			return fmt.Errorf("clone repository: %w", err)
		}
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	var recoveredTaskIDs []string
	if ids, err := st.RecoverFromRestart(context.Background()); err != nil {
		logger.Error("failed to recover runs after restart", "error", err.Error())
	} else if len(ids) > 0 {
		logger.Warn("recovered runs left running by a previous instance, resuming them", "count", len(ids), "task_ids", ids)
		recoveredTaskIDs = ids
	}

	cuClient := clickup.NewClient(cfg.CUAPIToken, cfg.CUTeamID, clickup.WithLogger(logger))
	defer cuClient.Close()

	notifier := slack.NewNotifier(cfg.SlackWebhookURL)
	runner := review.NewRunner(cfg.RepoPath, cfg.Home)

	q := queue.New(queue.Deps{
		ClickUp: cuClient,
		Store:   st,
		Slack:   notifier,
		Runner:  runner,
		Cfg:     cfg,
		Logger:  logger,
	}, 100)
	q.Start(cfg.WorkerConcurrency)

	// Прогоны, прерванные предыдущим падением/убийством процесса, доводятся
	// до конца в первую очередь, независимо от того, как сейчас выглядит
	// карточка задачи в ClickUp (см. Queue.SubmitResume).
	for _, taskID := range recoveredTaskIDs {
		q.SubmitResume(taskID)
	}

	mux := httpapi.NewMux(httpapi.Deps{
		Queue:         q,
		Trigger:       &manualRunner{cfg: cfg, cuClient: cuClient, queue: q, logger: logger},
		WebhookSecret: cfg.CUWebhookSecret,
		Store:         st,
		RepoPath:      cfg.RepoPath,
		ClaudeBinary:  "claude",
		Logger:        logger,
	})

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: mux,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "port", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	stopReconcile := make(chan struct{})
	if cfg.ReconcileInterval > 0 {
		go runReconcileLoop(cfg, cuClient, q, logger, stopReconcile)
	}

	// Обычный signal.Notify, а не NotifyContext: NotifyContext перехватывает
	// сигнал только один раз и после первого срабатывания возвращает его
	// обработку в дефолтный режим — повторный SIGTERM (обычное дело при
	// docker stop/пересоздании через Compose) тогда убивает процесс мгновенно
	// и обрывает текущий прогон ревью, даже не дав graceful shutdown начаться
	// толком. Регистрация здесь остаётся активной до конца жизни процесса,
	// поэтому любые последующие SIGTERM/SIGINT продолжают уходить в канал,
	// а не в дефолтный обработчик ОС.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case <-sigCh:
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		logger.Error("http server failed", "error", err.Error())
	}

	close(stopReconcile)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server shutdown error", "error", err.Error())
	}

	q.Shutdown(cfg.ReviewTimeout)

	return nil
}

// runReconcileLoop — сверка (Требование 1.2): раз в ReconcileInterval
// запрашивает задачи списка с нужным тегом и статусом и ставит в очередь
// всё, что ещё не обрабатывалось. Закрывает класс проблем с потерянными
// вебхуками.
func runReconcileLoop(cfg *config.Config, cuClient *clickup.Client, q *queue.Queue, logger *slog.Logger, stop <-chan struct{}) {
	ticker := time.NewTicker(cfg.ReconcileInterval)
	defer ticker.Stop()

	reconcileOnce(cfg, cuClient, q, logger)

	for {
		select {
		case <-ticker.C:
			reconcileOnce(cfg, cuClient, q, logger)
		case <-stop:
			return
		}
	}
}

func reconcileOnce(cfg *config.Config, cuClient *clickup.Client, q *queue.Queue, logger *slog.Logger) {
	if _, err := scanAndSubmit(context.Background(), cfg, cuClient, q); err != nil {
		logger.Error("reconcile: failed to list tasks", "error", err.Error())
		return
	}
}

// scanAndSubmit запрашивает задачи списка с нужным тегом и статусом и
// ставит в очередь всё, что ещё не обрабатывалось. Используется и сверкой,
// и ручным запуском через POST /api/run без task_id — это и есть та самая
// «одна функция постановки в очередь», в которую ведут оба источника.
func scanAndSubmit(ctx context.Context, cfg *config.Config, cuClient *clickup.Client, q *queue.Queue) ([]string, error) {
	scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tasks, err := cuClient.ListTasksByTagAndStatus(scanCtx, cfg.CUListID, cfg.TriggerTag, cfg.StatusTrigger)
	if err != nil {
		return nil, err
	}

	if len(tasks) == 0 {
		slog.Info("сверка: подходящих задач не найдено", "list_id", cfg.CUListID, "tag", cfg.TriggerTag, "status", cfg.StatusTrigger)
		return nil, nil
	}

	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		q.Submit(t.ID)
		ids = append(ids, t.ID)
	}
	return ids, nil
}

// manualRunner реализует httpapi.ManualRunner: ручной запуск конкретной
// задачи (если передан task_id) или немедленное пересканирование доски
// (та же логика, что и у сверки).
type manualRunner struct {
	cfg      *config.Config
	cuClient *clickup.Client
	queue    *queue.Queue
	logger   *slog.Logger
}

func (m *manualRunner) RunNow(ctx context.Context, taskID string) ([]string, error) {
	if taskID != "" {
		m.queue.Submit(taskID)
		return []string{taskID}, nil
	}
	ids, err := scanAndSubmit(ctx, m.cfg, m.cuClient, m.queue)
	if err != nil {
		return nil, err
	}
	return ids, nil
}
