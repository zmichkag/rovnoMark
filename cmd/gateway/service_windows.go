//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

type gatewayService struct {
	serverRunner func(ctx context.Context, elog *eventlog.Log) error
	name         string
}

func (s *gatewayService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	elog, err := eventlog.Open(s.name)
	if err == nil {
		defer elog.Close()
		_ = elog.Info(1, fmt.Sprintf("Служба %s запускается", s.name))
	} else {
		slog.Warn("Не удалось открыть Windows Event Log", "err", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverStopped := make(chan error, 1)
	go func() {
		serverStopped <- s.serverRunner(ctx, elog)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for {
		select {
		case err := <-serverStopped:
			if err != nil && elog != nil {
				_ = elog.Error(101, fmt.Sprintf("Аварийное завершение рабочего цикла шлюза: %v", err))
			}
			changes <- svc.Status{State: svc.StopPending}
			return false, 1

		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				if elog != nil {
					_ = elog.Info(2, "Получен сигнал остановки службы. Инициирован Graceful Shutdown...")
				}

				// Сигнализируем рабочему контуру о завершении
				cancel()

				// Ожидаем остановки HTTP-сервера, сброса WAL и закрытия пула соединений
				select {
				case <-serverStopped:
					if elog != nil {
						_ = elog.Info(3, "Ресурсы освобождены, служба штатно завершила работу")
					}
				case <-time.After(8 * time.Second):
					if elog != nil {
						_ = elog.Warning(4, "Превышен таймаут ожидания Graceful Shutdown при остановке службы")
					}
				}
				return false, 0
			}
		}
	}
}

func isWindowsService() bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return isSvc
}

func runWindowsService(name string, runner func(ctx context.Context, elog *eventlog.Log) error) error {
	s := &gatewayService{
		serverRunner: runner,
		name:         name,
	}
	return svc.Run(name, s)
}
