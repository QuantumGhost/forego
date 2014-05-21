package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const shutdownGraceTime = 3 * time.Second

var flagPort int
var flagConcurrency string
var flagRestart bool

var cmdStart = &Command{
	Run:   runStart,
	Usage: "start [process name] [-f procfile] [-e env] [-c concurrency] [-p port] [-r]",
	Short: "Start the application",
	Long: `
Start the application specified by a Procfile (defaults to ./Procfile)

Examples:

  forego start
  forego start web
  forego start -f Procfile.test -e .env.test
`,
}

func init() {
	cmdStart.Flag.StringVar(&flagProcfile, "f", "Procfile", "procfile")
	cmdStart.Flag.StringVar(&flagEnv, "e", "", "env")
	cmdStart.Flag.IntVar(&flagPort, "p", 5000, "port")
	cmdStart.Flag.StringVar(&flagConcurrency, "c", "", "concurrency")
	cmdStart.Flag.BoolVar(&flagRestart, "r", false, "restart")
}

func parseConcurrency(value string) (map[string]int, error) {
	concurrency := map[string]int{}
	if strings.TrimSpace(value) == "" {
		return concurrency, nil
	}

	parts := strings.Split(value, ",")
	for _, part := range parts {
		if !strings.Contains(part, "=") {
			return concurrency, errors.New("Parsing concurency")
		}

		nameValue := strings.Split(part, "=")
		n, v := strings.TrimSpace(nameValue[0]), strings.TrimSpace(nameValue[1])
		if n == "" || v == "" {
			return concurrency, errors.New("Parsing concurency")
		}

		numProcs, err := strconv.ParseInt(v, 10, 16)
		if err != nil {
			return concurrency, err
		}

		concurrency[n] = int(numProcs)
	}
	return concurrency, nil
}

type Forego struct {
	shutdown    sync.Once     // Closes teardown exactly once
	teardown    chan struct{} // barrier: closed when shutting down
	teardownNow chan struct{} // barrier: second CTRL-C. More urgent.

	wg sync.WaitGroup
}

func (f *Forego) SignalShutdown() {
	f.shutdown.Do(func() { close(f.teardown) })
}

func (f *Forego) monitorInterrupt() {
	handler := make(chan os.Signal, 1)
	signal.Notify(handler, os.Interrupt)

	first := true
	var once sync.Once

	for sig := range handler {
		switch sig {
		case os.Interrupt:
			fmt.Println("      | ctrl-c detected")

			if !first {
				once.Do(func() { close(f.teardownNow) })
			}
			f.SignalShutdown()
			first = false
		}
	}
}

func (f *Forego) startProcess(idx, procNum int, proc ProcfileEntry, env Env, of *OutletFactory) {
	port := flagPort + (idx * 100)

	ps := NewProcess(proc.Command, env)
	procName := fmt.Sprint(proc.Name, ".", procNum+1)
	ps.Env["PORT"] = strconv.Itoa(port)
	ps.Root = filepath.Dir(flagProcfile)
	ps.Stdin = nil
	ps.Stdout = of.CreateOutlet(procName, idx, false)
	ps.Stderr = of.CreateOutlet(procName, idx, true)

	of.SystemOutput(fmt.Sprintf("starting %s on port %d", procName, port))

	finished := make(chan struct{}) // closed on process exit

	ps.Start()
	go func() {
		defer close(finished)
		ps.Wait()
	}()

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()

		// Prevent goroutine from exiting before process has finished.
		defer func() { <-finished }()

		select {
		case <-finished:
			if flagRestart {
				f.startProcess(idx, procNum, proc, env, of)
				return
			} else {
				f.SignalShutdown()
			}

		case <-f.teardown:
			// Forego tearing down

			if !osHaveSigTerm {
				of.SystemOutput(fmt.Sprintf("Killing %s", procName))
				ps.cmd.Process.Kill()
				return
			}

			of.SystemOutput(fmt.Sprintf("sending SIGTERM to %s", procName))
			ps.SendSigTerm()

			// Give the process a chance to exit, otherwise kill it.
			select {
			case <-time.After(shutdownGraceTime):
				of.SystemOutput(fmt.Sprintf("Killing %s", procName))
				ps.SendSigKill()
			case <-f.teardownNow:
				of.SystemOutput(fmt.Sprintf("Killing %s", procName))
				ps.SendSigKill()
			case <-finished:
			}
		}
	}()
}

func runStart(cmd *Command, args []string) {
	root := filepath.Dir(flagProcfile)

	if flagEnv == "" {
		flagEnv = filepath.Join(root, ".env")
	}

	pf, err := ReadProcfile(flagProcfile)
	handleError(err)

	env, err := ReadEnv(flagEnv)
	handleError(err)

	concurrency, err := parseConcurrency(flagConcurrency)
	handleError(err)

	of := NewOutletFactory()
	of.Padding = pf.LongestProcessName()

	f := &Forego{
		teardown:    make(chan struct{}),
		teardownNow: make(chan struct{}),
	}

	go f.monitorInterrupt()

	var singleton string = ""
	if len(args) > 0 {
		singleton = args[0]
		if !pf.HasProcess(singleton) {
			of.ErrorOutput(fmt.Sprintf("no such process: %s", singleton))
		}
	}

	for idx, proc := range pf.Entries {
		numProcs := 1
		if value, ok := concurrency[proc.Name]; ok {
			numProcs = value
		}
		for i := 0; i < numProcs; i++ {
			if (singleton == "") || (singleton == proc.Name) {
				f.startProcess(idx, i, proc, env, of)
			}
		}
	}

	<-f.teardown
	of.SystemOutput("shutting down")

	f.wg.Wait()
}
