package logger

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zerologr"
	"github.com/rs/zerolog"
)

var (
	sharedInstance logr.Logger
	oncer          sync.Once
)

func Shared() logr.Logger {
	oncer.Do(func() {
		sharedInstance = NewZeroLogr()
	})
	return sharedInstance
}

func Init(level string) {
	logLevel, err := zerolog.ParseLevel(level)
	if err != nil {
		panic(err)
	}

	zerolog.SetGlobalLevel(logLevel)
}

// NewZeroLogr returns a Zerolog logger wrapped in a go-logr/logr interface.
func NewZeroLogr() logr.Logger {
	zl := zerolog.New(consoleWriter()).With().Timestamp().Logger()
	return zerologr.New(&zl)
}

func consoleWriter() zerolog.ConsoleWriter {
	output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
	output.FormatLevel = func(i interface{}) string {
		return strings.ToUpper(fmt.Sprintf("| %-6s|", i))
	}

	output.FormatMessage = func(i interface{}) string {
		return fmt.Sprintf("%+v", i)
	}

	output.FormatFieldName = func(i interface{}) string {
		return fmt.Sprintf("%s:", i)
	}

	output.FormatFieldValue = func(i interface{}) string {
		return fmt.Sprintf("%s", i)
	}

	return output
}
