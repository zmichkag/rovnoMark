package main

// EventLogger — кроссплатформенный интерфейс для системного логгера службы
type EventLogger interface {
	Info(eid uint32, msg string) error
	Warning(eid uint32, msg string) error
	Error(eid uint32, msg string) error
}
