// Recall is an ACP coding harness with durable, source-linked project memory.
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/providers"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/settings"
	"github.com/unreallabsai/unreal-agent/recall/acp"
	"github.com/unreallabsai/unreal-agent/recall/memory"
)

type factory struct {
	provider providers.Provider
	base     string
}

func (f factory) client() (providers.Client, error) {
	key, err := f.provider.APIKey(os.Getenv)
	if err != nil {
		return nil, err
	}
	return f.provider.NewClient(key, f.base, 3, os.Getenv)
}
func (f factory) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	client, err := f.client()
	if err != nil {
		return llm.Response{}, err
	}
	defer client.Close()
	return client.Respond(ctx, request, options)
}

func stateDirectory() (string, error) {
	if dir := os.Getenv("RECALL_DATA_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		if root := os.Getenv("LOCALAPPDATA"); root != "" {
			return filepath.Join(root, "Recall"), nil
		}
		return filepath.Join(home, "AppData", "Local", "Recall"), nil
	}
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		return filepath.Join(root, "recall"), nil
	}
	return filepath.Join(home, ".local", "state", "recall"), nil
}
func envInt(name string, fallback int) (int, error) {
	text := os.Getenv(name)
	if text == "" {
		return fallback, nil
	}
	number, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return number, nil
}
func config() (acp.Config, error) {
	dir, err := stateDirectory()
	if err != nil {
		return acp.Config{}, err
	}
	providerName := os.Getenv("RECALL_PROVIDER")
	if providerName == "" {
		providerName = "openai-codex"
	}
	provider, err := providers.Find(providers.Default(), providerName)
	if err != nil {
		return acp.Config{}, err
	}
	base := os.Getenv("RECALL_BASE_URL")
	if base == "" {
		base = provider.BaseURL
	}
	model := os.Getenv("RECALL_MODEL")
	if model == "" {
		if providerName == "openai-codex" || providerName == "openai" {
			model = "gpt-6.1-sol"
		} else {
			model = provider.DefaultModel
		}
	}
	if model == "" {
		return acp.Config{}, errors.New("RECALL_MODEL is required for this provider")
	}
	effort := llm.ReasoningEffort(os.Getenv("RECALL_REASONING_EFFORT"))
	if effort == "" {
		effort = llm.ReasoningEffortHigh
	}
	if !effort.Valid() {
		return acp.Config{}, errors.New("invalid RECALL_REASONING_EFFORT")
	}
	limits := memory.DefaultConfig()
	for _, entry := range []struct {
		name  string
		value *int
	}{{"RECALL_SCAN_TOKENS", &limits.ScanTokens}, {"RECALL_COMPACT_TOKENS", &limits.CompactTokens}, {"RECALL_CONTEXT_TOKENS", &limits.ContextTokens}, {"RECALL_RESERVE_TOKENS", &limits.ReserveTokens}, {"RECALL_RETAIN_TOKENS", &limits.RetainTokens}, {"RECALL_SUMMARY_BYTES", &limits.SummaryBytes}, {"RECALL_LAYER_FANOUT", &limits.Fanout}} {
		*entry.value, err = envInt(entry.name, *entry.value)
		if err != nil {
			return acp.Config{}, err
		}
	}
	if err = limits.Validate(); err != nil {
		return acp.Config{}, err
	}
	f := factory{provider: provider, base: base}
	models := []string{model}
	configured, err := settings.Load("")
	if err == nil {
		if p, ok := configured.Providers[providerName]; ok {
			models = nil
			for _, m := range p.Models {
				models = append(models, m.ID)
			}
			found := false
			for _, id := range models {
				found = found || id == model
			}
			if !found {
				models = append(models, model)
			}
		}
	}
	if custom := os.Getenv("RECALL_MODELS"); custom != "" {
		models = strings.Split(custom, ",")
	}
	return acp.Config{DataDirectory: dir, Model: model, Models: models, Effort: effort, Memory: limits, Client: f, AuthCheck: func() error {
		client, err := f.client()
		if err != nil {
			return err
		}
		return client.Close()
	}}, nil
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Println("Recall harness\n\nUsage:\n  recall acp                 Connect as a local ACP provider in T3 Code\n  recall doctor              Check the local model login and memory settings\n  recall version             Print the installed version\n  recall memory status       Show project memory state\n  recall memory search       Search memory and original transcripts\n  recall memory read         Read a source reference\n  recall memory verify       Check all immutable project objects\n  recall memory worker       Process pending memory updates\n  recall memory projects     List memory project identities\n  recall memory export       Stream project memory to stdout\n  recall memory import       Import a memory stream from stdin\n  recall memory inventory    List immutable object references\n\nMemory commands accept --workspace PATH. Configure RECALL_PROVIDER, RECALL_MODEL,\nRECALL_DATA_DIR, and optional RECALL_*_TOKENS limits through the environment.")
		return nil
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println("Recall " + acp.Version)
		return nil
	}
	c, err := config()
	if err != nil {
		return err
	}
	switch args[0] {
	case "acp":
		flags := flag.NewFlagSet("recall acp", flag.ContinueOnError)
		trace := flags.String("trace", "", "optional protocol metadata log; excludes content")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *trace != "" {
			if err := os.Setenv("RECALL_TRACE_PATH", *trace); err != nil {
				return err
			}
		}
		return acp.Serve(ctx, os.Stdin, os.Stdout, c)
	case "doctor":
		authErr := c.AuthCheck()
		status := map[string]any{"name": "Recall", "version": acp.Version, "platform": runtime.GOOS + "/" + runtime.GOARCH, "model": c.Model, "data_directory": c.DataDirectory, "authentication_configured": authErr == nil, "memory": c.Memory, "token_estimate": "o200k_base with framing and output reserves; actual provider usage checked after calls"}
		if authErr != nil {
			status["authentication_error"] = authErr.Error()
		}
		return json.NewEncoder(os.Stdout).Encode(status)
	case "memory":
		return memoryCommand(ctx, c, args[1:])
	default:
		return fmt.Errorf("unknown Recall command %q", args[0])
	}
}

func memoryCommand(ctx context.Context, c acp.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("choose a memory operation")
	}
	if args[0] == "import" {
		reader := bufio.NewReader(os.Stdin)
		var input io.Reader = reader
		if magic, _ := reader.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
			compressed, err := gzip.NewReader(reader)
			if err != nil {
				return err
			}
			defer compressed.Close()
			input = compressed
		}
		changed, err := memory.Import(ctx, filepath.Join(c.DataDirectory, "memory"), input)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"changed_threads": changed})
	}
	if args[0] == "projects" {
		entries, err := os.ReadDir(filepath.Join(c.DataDirectory, "memory", "projects"))
		if errors.Is(err, os.ErrNotExist) {
			entries = nil
		} else if err != nil {
			return err
		}
		projects := []map[string]string{}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(c.DataDirectory, "memory", "projects", entry.Name(), "project.json"))
			if err != nil {
				continue
			}
			var manifest memory.Project
			if json.Unmarshal(data, &manifest) == nil {
				var heads []string
				for id, t := range manifest.Threads {
					heads = append(heads, id+":"+memory.PrefixDigest(t.Inputs)+":"+t.Prepared.Memory)
				}
				sort.Strings(heads)
				projects = append(projects, map[string]string{"id": entry.Name(), "key": manifest.Key, "fingerprint": memory.PrefixDigest(heads)})
			}
		}
		return json.NewEncoder(os.Stdout).Encode(projects)
	}
	if args[0] == "worker" {
		flags := flag.NewFlagSet("recall memory worker", flag.ContinueOnError)
		once := flags.Bool("once", false, "process pending memory once")
		interval := flags.Duration("interval", 5*time.Second, "idle scan interval")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		model := llm.Model{ID: c.Model, ReasoningEffort: c.Effort}
		if *once {
			count, err := memory.WorkerOnce(ctx, filepath.Join(c.DataDirectory, "memory"), c.Memory, c.Client, model)
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"completed": count, "ok": err == nil})
			return err
		}
		return memory.Worker(ctx, filepath.Join(c.DataDirectory, "memory"), c.Memory, c.Client, model, *interval, func(count int, err error) {
			if count > 0 {
				fmt.Fprintf(os.Stderr, "Recall memory: completed %d pending thread updates\n", count)
			}
			if err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "Recall memory: an update failed; sources are retained and retry details are in project memory status")
			}
		})
	}
	flags := flag.NewFlagSet("recall memory "+args[0], flag.ContinueOnError)
	cwd := flags.String("workspace", ".", "project workspace")
	query := flags.String("query", "", "search text")
	ref := flags.String("reference", "", "immutable source reference")
	limit := flags.Int("limit", 10, "maximum search results")
	key := flags.String("project-key", "", "explicit project identity for memory replication")
	compressed := flags.Bool("gzip", false, "compress a memory export")
	knownStdin := flags.Bool("known-stdin", false, "read the receiver's known object references from stdin")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	absolute, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	projectKey := *key
	if projectKey == "" {
		projectKey = acp.ProjectKey(absolute)
	}
	store, err := memory.Open(filepath.Join(c.DataDirectory, "memory"), projectKey)
	if err != nil {
		return err
	}
	var value any
	switch args[0] {
	case "inventory":
		refs, err := store.Inventory()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(refs)
	case "export":
		known := map[string]bool{}
		if *knownStdin {
			var refs []string
			if err := json.NewDecoder(os.Stdin).Decode(&refs); err != nil {
				return err
			}
			for _, ref := range refs {
				known[ref] = true
			}
		}
		if *compressed {
			writer := gzip.NewWriter(os.Stdout)
			if err := store.ExportKnown(ctx, writer, known); err != nil {
				_ = writer.Close()
				return err
			}
			return writer.Close()
		}
		return store.ExportKnown(ctx, os.Stdout, known)
	case "status":
		p, err := store.Snapshot(ctx)
		if err != nil {
			return err
		}
		problems := map[string]any{}
		pending := 0
		for id, t := range p.Threads {
			if t.LastError != "" {
				problems[id] = map[string]any{"error": t.LastError, "retry_after": t.RetryAfter}
			}
			if t.Prepared.Through < len(t.Inputs) || t.ProjectMemory != t.Prepared.Memory {
				pending++
			}
		}
		value = map[string]any{"project_id": store.ID, "project_key": store.Key, "revision": p.Revision, "threads": len(p.Threads), "overview": p.Overview, "layers": p.Levels, "pending_threads": pending, "problems": problems}
	case "search":
		value, err = store.Search(ctx, *query, *limit)
	case "read":
		var data []byte
		data, err = store.Read(*ref)
		if err == nil {
			_, err = os.Stdout.Write(append(data, '\n'))
		}
		return err
	case "verify":
		entries, readErr := os.ReadDir(filepath.Join(store.Directory, "objects"))
		if readErr != nil {
			return readErr
		}
		count := 0
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			stem := strings.TrimSuffix(entry.Name(), ".json")
			kind, hash, found := strings.Cut(stem, "-")
			if !found {
				return errors.New("invalid memory object name")
			}
			if _, err := store.Read("recall://" + store.ID + "/" + kind + "/" + hash); err != nil {
				return fmt.Errorf("verify %s: %w", entry.Name(), err)
			}
			count++
		}
		value = map[string]any{"project_id": store.ID, "verified_objects": count, "time": time.Now().UTC()}
	default:
		return errors.New("unknown memory operation")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(value)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Recall:", err)
		os.Exit(1)
	}
}
