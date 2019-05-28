package que

import "log"

type Logger interface {
	Printf(format string, v ...interface{})
	Println(v ...interface{})
}

type stdLogger struct{}

func (l *stdLogger) Printf(format string, v ...interface{}) {
	log.Printf(format, v...)
}

func (l *stdLogger) Println(v ...interface{}) {
	log.Println(v...)
}
