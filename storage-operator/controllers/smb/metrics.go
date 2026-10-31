package smb

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics keeps the six metric names the Python operator exposed on /metrics. They are now
// served from the controller-runtime registry (port 8080, /metrics) next to the standard
// controller_runtime_* series for controller="smb".
type Metrics struct {
	ReconcileTotal  prometheus.Counter
	ReconcileErrors prometheus.Counter
	SharesManaged   prometheus.Gauge
	LastReconcile   prometheus.Gauge
	RetryCount      prometheus.Counter
	EventsSent      prometheus.Counter
}

// NewMetrics creates the collectors and registers them on reg (nil = do not register).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ReconcileTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "smb_operator_reconcile_total", Help: "Total number of reconciliations"}),
		ReconcileErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "smb_operator_reconcile_errors_total", Help: "Total number of reconciliation errors"}),
		SharesManaged: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "smb_operator_shares_managed", Help: "Current number of shares being managed"}),
		LastReconcile: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "smb_operator_last_reconcile_timestamp_seconds", Help: "Timestamp of last reconciliation"}),
		RetryCount: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "smb_operator_retry_count_total", Help: "Total number of API call retries"}),
		EventsSent: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "smb_operator_events_sent_total", Help: "Total number of Kubernetes events sent"}),
	}
	if reg != nil {
		reg.MustRegister(m.ReconcileTotal, m.ReconcileErrors, m.SharesManaged, m.LastReconcile, m.RetryCount, m.EventsSent)
	}
	return m
}
