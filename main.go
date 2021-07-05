package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jpillora/backoff"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gitlab.com/signald/signald-go/cmd/signaldctl/common"
	"gitlab.com/signald/signald-go/signald"
	v1 "gitlab.com/signald/signald-go/signald/client-protocol/v1"
)

const (
	defaultSocketPath = "/var/run/signald/signald.sock"
)

var (
	flagListen = flag.String("listen", ":9716", "[ip]:port to listen on for HTTP")
	flagSocket = flag.String("signald", defaultSocketPath, "UNIX socket to connect to signald on")
	flagConfig = flag.String("config", "", "YAML configuration filename")

	// signalClient    *signald.Client
	cfg       *Config
	receivers = map[string]*Receiver{}
	templates *template.Template
	connected bool
)

var (
	receivedMetric = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "signald_webhook",
			Subsystem: "alerts",
			Name:      "received_total",
		})
	errorsMetric = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "signald_webhook",
			Subsystem: "alerts",
			Name:      "errors_total",
		}, []string{"type"})

	signaldInfoMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "signald_webhook",
			Subsystem: "signal",
			Name:      "info",
		}, []string{"name", "version"})
)

func init() {
	err := prometheus.Register(receivedMetric)
	if err != nil {
		log.Fatal("unable to register metric `received_total`")
	}
	err = prometheus.Register(errorsMetric)
	if err != nil {
		log.Fatal("unable to register metric `errors_total`")
	}
	err = prometheus.Register(signaldInfoMetric)
	if err != nil {
		log.Fatal("unable to register metric `info`")
	}
	for _, errorType := range []string{"decode", "handler"} {
		errorsMetric.With(prometheus.Labels{"type": errorType}).Add(0)
	}
}

func handleReconnect() {
	b := &backoff.Backoff{}
	for {
		common.Signald = &signald.Signald{SocketPath: *flagSocket}
		if err := common.Signald.Connect(); err != nil {
			connected = false
			d := b.Duration()
			log.Printf("unable to connect: %s, retry in %s", err, d)
			time.Sleep(d)
			continue
		}
		b.Reset()
		connected = true
		common.Signald.Listen(nil)

	}
}

func main() {
	flag.Parse()
	if *flagConfig == "" {
		flag.Usage()
	}

	var err error
	cfg, err = LoadFile(*flagConfig)
	if err != nil {
		log.Fatal(err)
	}

	if len(cfg.Receivers) == 0 {
		log.Fatal(*flagConfig, ": no receivers defined")
	}

	for _, recv := range cfg.Receivers {
		if len(recv.Name) == 0 {
			log.Fatal("Receiver missing 'name:'")
		}
		if _, ok := receivers[recv.Name]; ok {
			log.Fatalf("Duplicate receiver name: %q", recv.Name)
		}
		receivers[recv.Name] = recv
	}

	templates, err = template.FromGlobs(cfg.Templates...)
	if err != nil {
		log.Fatalf("Error parsing templates: %v", err)
	}

	err = prometheus.Register(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: "signald_webhook",
			Subsystem: "signal",
			Name:      "connected",
			Help:      "True if connected to signald.",
		},
		func() float64 {
			if connected {
				return 1
			}
			return 0
		},
	))
	if err != nil {
		log.Fatal("unable to register metric `connected`")
	}

	go handleReconnect()

	http.HandleFunc("/alert", hook)
	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(*flagListen, nil))
}

func hook(w http.ResponseWriter, req *http.Request) {
	receivedMetric.Add(1)
	var m Message
	err := json.NewDecoder(req.Body).Decode(&m)
	if err != nil {
		log.Printf("Decoding /alert failed: %v", err)
		http.Error(w, "Decode failed", http.StatusBadRequest)
		errorsMetric.With(prometheus.Labels{"type": "decode"}).Add(1)
		return
	}
	err = handle(&m)
	if err != nil {
		log.Print(err)
		http.Error(w, "Handling alert failed", http.StatusInternalServerError)
		errorsMetric.With(prometheus.Labels{"type": "handle"}).Add(1)
	} else {
		fmt.Fprintln(w, "ok")
	}
}

func handle(m *Message) error {

	recv, ok := receivers[m.Receiver]
	if !ok {
		return fmt.Errorf("%q: Receiver not configured", m.Receiver)
	}
	log.Printf("Send via %q: %#v", m.Receiver, recv)

	body, err := templates.ExecuteTextString(recv.Template, m)
	if err != nil {
		body = fmt.Sprintf("%#v: Template expansion failed: %v", m.GroupLabels, err)
	}

	for _, toTmpl := range recv.To {
		req := v1.SendRequest{
			Username:    recv.Sender,
			MessageBody: body,
		}

		var to string
		to, err = templates.ExecuteTextString(toTmpl, m)
		if err != nil {
			log.Printf("Error executing to template: %q: %v", toTmpl, err)
			continue
		}
		if strings.HasPrefix(to, "tel:") {
			req.RecipientAddress = &v1.JsonAddress{Number: to[4:]}
		} else if strings.HasPrefix(to, "group:") {
			req.RecipientGroupID = to[6:]
		} else {
			log.Printf("Unknown to: %q, expected tel:+number or group:id", to)
		}
		_, err := req.Submit(common.Signald)
		if err != nil {
			log.Fatal("error sending request to signald: ", err)
		}

	}
	return err
}
