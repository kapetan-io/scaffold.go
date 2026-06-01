package scaffold_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/kapetan-io/scaffold.go"
)

// exampleDaemon is a minimal service that exposes a single /hello endpoint on
// the "api" binding. Dependencies live in the Inject struct so tests can
// substitute fakes without changing production code.
type exampleDaemon struct {
	Inject exampleInjectables
}

type exampleInjectables struct {
	Greeter func() string
}

func (d *exampleDaemon) OnStart(_ context.Context, sc *scaffold.DaemonConfig) error {
	if d.Inject.Greeter == nil {
		d.Inject.Greeter = func() string { return "hello, world" }
	}

	api := sc.Bindings.Add("api", sc.Config.IntOr("API_PORT", 8080))
	api.UseMiddleware(scaffold.PanicRecovery(sc.Log))

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, d.Inject.Greeter())
	})
	mux.Handle("/readyz", scaffold.ReadyHandler(func(_ context.Context) (bool, string) {
		return true, ""
	}))
	api.SetMux(mux)
	return nil
}

func (d *exampleDaemon) OnStop(_ context.Context) error { return nil }

// Example demonstrates the surface-testing pattern: start the daemon through
// its public entry point, exercise it via real HTTP, then shut it down. The
// Greeter dependency is injected with a fake so the test exercises the full
// middleware stack without any production side effects.
func Example() {
	ctx := context.Background()
	daemon := &exampleDaemon{
		Inject: exampleInjectables{
			Greeter: func() string { return "hello from test" },
		},
	}

	inst, err := scaffold.Start(ctx, daemon, &scaffold.Options{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		fmt.Println("start failed:", err)
		return
	}
	defer func() { _ = inst.Stop(ctx) }()

	resp, err := http.Get("http://" + inst.Addr("api").String() + "/hello")
	if err != nil {
		fmt.Println("request failed:", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println(resp.StatusCode, string(body))

	// Output: 200 hello from test
}

// ExampleServe shows the production main() wiring. The snippet is not run as
// part of the test suite because Serve blocks on SIGTERM / SIGINT.
func ExampleServe() {
	_ = func() {
		os.Exit(scaffold.Serve(
			context.Background(),
			os.Args,
			&exampleDaemon{},
			scaffold.Options{},
		))
	}
}
