// Command traceweave is the CLI for the traceweave tracer, demo service,
// replay engine and measured experiment.
//
// Usage:
//
//	traceweave experiment [--n 120] [--seed 20261023] [--dir DIR] [--out results.json]
//	traceweave serve      [--addr :8080] [--dir DIR] [--store traces.jsonl]
//	traceweave tree       --store traces.jsonl <trace-id>
//	traceweave span       --store traces.jsonl <trace-id> <span-name>
//	traceweave replay     --store traces.jsonl <trace-id>
//	traceweave surgery    --store traces.jsonl <trace-id> (--rows rows.json --span db.get_product | --template tmpl.txt | --seed N)
//	traceweave attribute  --store traces.jsonl --truth truth.json <trace-id>
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"traceweave/experiment"
	"traceweave/minidb"
	"traceweave/replay"
	"traceweave/service"
	"traceweave/store"
	"traceweave/trace"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "experiment":
		err = cmdExperiment(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "tree":
		err = cmdTree(os.Args[2:])
	case "span":
		err = cmdSpan(os.Args[2:])
	case "replay":
		err = cmdReplay(os.Args[2:])
	case "surgery":
		err = cmdSurgery(os.Args[2:])
	case "attribute":
		err = cmdAttribute(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: traceweave <experiment|serve|tree|span|replay|surgery|attribute> [flags] [args]")
}

func cmdExperiment(args []string) error {
	fs := flag.NewFlagSet("experiment", flag.ExitOnError)
	n := fs.Int("n", 120, "number of requests in the workload")
	seed := fs.Int64("seed", 20261023, "workload RNG seed")
	dir := fs.String("dir", "run", "output directory (db, traces.jsonl)")
	out := fs.String("out", "", "also write results JSON here")
	fs.Parse(args)
	res, err := experiment.Run(*dir, *n, *seed)
	if err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(raw))
	if *out != "" {
		if err := os.WriteFile(*out, append(raw, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", *out)
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dir := fs.String("dir", "demo", "data directory")
	storePath := fs.String("store", "", "span store path (default <dir>/traces.jsonl)")
	fs.Parse(args)
	dbDir := filepath.Join(*dir, "db")
	db, err := minidb.Open(dbDir)
	if err != nil {
		db, err = minidb.Create(dbDir, []*minidb.Table{experiment.Products(), experiment.Users()})
		if err != nil {
			return err
		}
	}
	sp := *storePath
	if sp == "" {
		sp = filepath.Join(*dir, "traces.jsonl")
	}
	st, err := store.New(sp)
	if err != nil {
		return err
	}
	svc := &service.Service{DB: db, Tracer: trace.NewTracer(st), Model: "mock-1"}
	fmt.Println("traceweave demo service on", *addr, "— try /answer?user=u1&product=p1")
	return http.ListenAndServe(*addr, svc.Handler())
}

func loadStoreAndTrace(args []string, extra func(*flag.FlagSet)) (string, *store.Store, *trace.Trace, error) {
	fs := flag.NewFlagSet("cmd", flag.ExitOnError)
	storePath := fs.String("store", "traces.jsonl", "span store path")
	if extra != nil {
		extra(fs)
	}
	fs.Parse(args)
	if fs.NArg() < 1 {
		return "", nil, nil, fmt.Errorf("missing trace id")
	}
	st, err := store.New(*storePath)
	if err != nil {
		return "", nil, nil, err
	}
	tr, err := st.LoadTrace(fs.Arg(0))
	if err != nil {
		return "", nil, nil, err
	}
	return fs.Arg(0), st, tr, nil
}

func cmdTree(args []string) error {
	_, _, tr, err := loadStoreAndTrace(args, nil)
	if err != nil {
		return err
	}
	root := tr.Root()
	if root == nil {
		return fmt.Errorf("trace has no root span")
	}
	var walk func(s *trace.Span, depth int)
	walk = func(s *trace.Span, depth int) {
		fmt.Printf("%s%s [%s] %s\n", strings.Repeat("  ", depth), s.Name, s.Kind, s.Duration())
		for _, c := range tr.Children(s.SpanID) {
			walk(c, depth+1)
		}
	}
	walk(root, 0)
	return nil
}

func cmdSpan(args []string) error {
	fs := flag.NewFlagSet("span", flag.ExitOnError)
	storePath := fs.String("store", "traces.jsonl", "span store path")
	fs.Parse(args)
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: span --store traces.jsonl <trace-id> <span-name>")
	}
	st, err := store.New(*storePath)
	if err != nil {
		return err
	}
	tr, err := st.LoadTrace(fs.Arg(0))
	if err != nil {
		return err
	}
	s := tr.ByName(fs.Arg(1))
	if s == nil {
		return fmt.Errorf("no span named %q", fs.Arg(1))
	}
	fmt.Printf("span %s (%s, %s, %s)\n", s.Name, s.SpanID, s.Kind, s.Duration())
	if len(s.Payload) > 0 {
		var buf bytes.Buffer
		if err := json.Indent(&buf, s.Payload, "", "  "); err == nil {
			fmt.Println(buf.String())
		} else {
			fmt.Println(string(s.Payload))
		}
	}
	return nil
}

func cmdReplay(args []string) error {
	_, _, tr, err := loadStoreAndTrace(args, nil)
	if err != nil {
		return err
	}
	res, err := replay.Replay(tr, nil)
	if err != nil {
		return err
	}
	fmt.Println("recorded:", res.RecordedOutput)
	fmt.Println("replayed:", res.Output)
	fmt.Println("byte-identical:", res.Identical)
	return nil
}

// scanFlags manually splits args into --name value pairs and positional
// args, in any order (Go's flag package stops at the first positional).
func scanFlags(args []string) (vals map[string]string, pos []string) {
	vals = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") && i+1 < len(args) {
			vals[strings.TrimPrefix(a, "--")] = args[i+1]
			i++
		} else {
			pos = append(pos, a)
		}
	}
	return vals, pos
}

func loadTrace(storePath, traceID string) (*trace.Trace, error) {
	st, err := store.New(storePath)
	if err != nil {
		return nil, err
	}
	return st.LoadTrace(traceID)
}

func cmdSurgery(args []string) error {
	vals, pos := scanFlags(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: surgery --store traces.jsonl <trace-id> (--rows rows.json [--span db.get_product] | --template tmpl.txt | --seed N)")
	}
	tr, err := loadTrace(vals["store"], pos[0])
	if err != nil {
		return err
	}
	rowsFile, tmplFile := vals["rows"], vals["template"]
	spanName := vals["span"]
	if spanName == "" {
		spanName = "db.get_product"
	}
	ov := &replay.Overrides{}
	switch {
	case rowsFile != "":
		raw, err := os.ReadFile(rowsFile)
		if err != nil {
			return err
		}
		var rows []map[string]string
		if err := json.Unmarshal(raw, &rows); err != nil {
			return err
		}
		ov.Rows = map[string][]map[string]string{spanName: rows}
	case tmplFile != "":
		raw, err := os.ReadFile(tmplFile)
		if err != nil {
			return err
		}
		ov.Template = string(raw)
	case vals["seed"] != "":
		var seed int64
		if _, err := fmt.Sscanf(vals["seed"], "%d", &seed); err != nil {
			return err
		}
		ov.Seed = &seed
	default:
		return fmt.Errorf("surgery needs one of --rows, --template, --seed")
	}
	before, err := replay.Replay(tr, nil)
	if err != nil {
		return err
	}
	after, err := replay.Replay(tr, ov)
	if err != nil {
		return err
	}
	fmt.Println("before surgery:", before.Output)
	fmt.Println("after surgery: ", after.Output)
	fmt.Println("answer changed:", before.Output != after.Output)
	return nil
}

func cmdAttribute(args []string) error {
	vals, pos := scanFlags(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: attribute --store traces.jsonl --truth truth.json <trace-id>")
	}
	tr, err := loadTrace(vals["store"], pos[0])
	if err != nil {
		return err
	}
	truthFile := vals["truth"]
	if truthFile == "" {
		return fmt.Errorf("attribute needs --truth truth.json")
	}
	raw, err := os.ReadFile(truthFile)
	if err != nil {
		return err
	}
	var tf struct {
		UserRow    map[string]string `json:"user_row"`
		ProductRow map[string]string `json:"product_row"`
		Template   string            `json:"template"`
	}
	if err := json.Unmarshal(raw, &tf); err != nil {
		return err
	}
	if tf.Template == "" {
		tf.Template = service.TemplateV1
	}
	judge := func(output string) bool {
		return strings.Contains(output, "$"+tf.ProductRow["price"]) &&
			strings.Contains(output, "currently "+tf.ProductRow["status"])
	}
	spanName, fix, err := replay.Attribute(tr, &replay.Truth{
		UserRow: tf.UserRow, ProductRow: tf.ProductRow, Template: tf.Template,
	}, judge)
	if err != nil {
		return err
	}
	fmt.Println("attributed span:", spanName)
	fmt.Println("log-only baseline guess:", replay.AttributeLogOnly(tr))
	if fix != nil {
		res, err := replay.Replay(tr, fix)
		if err != nil {
			return err
		}
		fmt.Println("after surgery at attributed span:", res.Output)
		fmt.Println("answer now correct:", judge(res.Output))
	}
	return nil
}
