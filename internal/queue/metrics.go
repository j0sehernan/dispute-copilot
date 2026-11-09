package queue

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var jobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "copilot_job_duration_seconds",
	Help:    "Time spent running a job, by kind and result.",
	Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
}, []string{"kind", "result"})

func observeJob(kind string, d time.Duration, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	jobDuration.WithLabelValues(kind, result).Observe(d.Seconds())
}
