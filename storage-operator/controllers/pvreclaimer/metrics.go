package pvreclaimer

import "github.com/prometheus/client_golang/prometheus"

// Result label values of storage_operator_pv_reclaimed_total.
const (
	ResultReclaimed = "reclaimed"
	ResultDryRun    = "dry_run"
	ResultSkipped   = "skipped"
	ResultError     = "error"
)

// Metrics holds the reclaimer's collectors.
type Metrics struct {
	Reclaimed *prometheus.CounterVec
}

// NewMetrics creates the collectors and registers them on reg (nil = do not register).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Reclaimed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "storage_operator_pv_reclaimed_total",
			Help: "Released PVs handled by the pv-reclaimer, by result (reclaimed, dry_run, skipped, error)",
		}, []string{"result"}),
	}
	for _, r := range []string{ResultReclaimed, ResultDryRun, ResultSkipped, ResultError} {
		m.Reclaimed.WithLabelValues(r)
	}
	if reg != nil {
		reg.MustRegister(m.Reclaimed)
	}
	return m
}
