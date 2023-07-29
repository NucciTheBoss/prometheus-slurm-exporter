package main

import (
	"encoding/json"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/exp/slog"
)

type NodeMetrics struct {
	Hostname     string   `json:"hostname"`
	Cpus         float64  `json:"cpus"`
	RealMemory   float64  `json:"real_memory"`
	FreeMemory   float64  `json:"free_memory"`
	Partitions   []string `json:"partitions"`
	State        string   `json:"state"`
	AllocMemory  float64  `json:"alloc_memory"`
	AllocCpus    float64  `json:"alloc_cpus"`
	IdleCpus     float64  `json:"idle_cpus"`
	Weight       float64  `json:"weight"`
	CpuLoad      float64  `json:"cpu_load"`
	Architecture string   `json:"architecture"`
}

type sinfoResponse struct {
	Meta   map[string]interface{} `json:"meta"`
	Errors []string               `json:"errors"`
	Nodes  []NodeMetrics          `json:"nodes"`
}

func parseNodeMetrics(jsonNodeList []byte) ([]NodeMetrics, error) {
	squeue := sinfoResponse{}
	err := json.Unmarshal(jsonNodeList, &squeue)
	if err != nil {
		slog.Error("Unmarshaling node metrics %q", err)
		return nil, err
	}
	if len(squeue.Errors) > 0 {
		for _, e := range squeue.Errors {
			slog.Error("Api error response %q", e)
		}
		return nil, errors.New(squeue.Errors[0])
	}
	return squeue.Nodes, nil
}

type PartitionMetrics struct {
	Cpus        float64
	RealMemory  float64
	FreeMemory  float64
	AllocMemory float64
	AllocCpus   float64
	CpuLoad     float64
	IdleCpus    float64
	Weight      float64
}

func fetchNodePartitionMetrics(nodes []NodeMetrics) map[string]*PartitionMetrics {
	partitions := make(map[string]*PartitionMetrics)
	for _, node := range nodes {
		for _, p := range node.Partitions {
			partition, ok := partitions[p]
			if !ok {
				partition = new(PartitionMetrics)
				partitions[p] = partition
			}
			partition.Cpus += node.Cpus
			partition.RealMemory += node.RealMemory
			partition.FreeMemory += node.FreeMemory
			partition.AllocMemory += node.AllocMemory
			partition.AllocCpus += node.AllocCpus
			partition.IdleCpus += node.IdleCpus
			partition.Weight += node.Weight
			partition.CpuLoad += node.CpuLoad
		}
	}
	return partitions
}

type CpuSummaryMetrics struct {
	Total    float64
	Idle     float64
	Load     float64
	PerState map[string]float64
}

func fetchNodeTotalCpuMetrics(nodes []NodeMetrics) *CpuSummaryMetrics {
	cpuSummaryMetrics := &CpuSummaryMetrics{
		PerState: make(map[string]float64),
	}
	for _, node := range nodes {
		cpuSummaryMetrics.Total += node.Cpus
		cpuSummaryMetrics.Idle += node.IdleCpus
		cpuSummaryMetrics.Load += node.CpuLoad
		cpuSummaryMetrics.PerState[node.State] += node.Cpus
	}
	return cpuSummaryMetrics
}

type MemSummaryMetrics struct {
	AllocMemory float64
	FreeMemory  float64
	RealMemory  float64
}

func fetchNodeTotalMemMetrics(nodes []NodeMetrics) *MemSummaryMetrics {
	memSummary := new(MemSummaryMetrics)
	for _, node := range nodes {
		memSummary.AllocMemory += node.AllocMemory
		memSummary.FreeMemory += node.FreeMemory
		memSummary.RealMemory += node.RealMemory
	}
	return memSummary
}

type NodesCollector struct {
	// collector state
	cache   *AtomicThrottledCache
	fetcher SlurmFetcher
	// partition summary metrics
	partitionCpus        *prometheus.Desc
	partitionRealMemory  *prometheus.Desc
	partitionFreeMemory  *prometheus.Desc
	partitionAllocMemory *prometheus.Desc
	partitionAllocCpus   *prometheus.Desc
	partitionIdleCpus    *prometheus.Desc
	partitionWeight      *prometheus.Desc
	partitionCpuLoad     *prometheus.Desc
	// cpu summary stats
	cpusPerState  *prometheus.Desc
	totalCpus     *prometheus.Desc
	totalIdleCpus *prometheus.Desc
	totalCpuLoad  *prometheus.Desc
	// memory summary stats
	totalRealMemory  *prometheus.Desc
	totalFreeMemory  *prometheus.Desc
	totalAllocMemory *prometheus.Desc
	// exporter metrics
	nodeScrapeErrors prometheus.Counter
}

func NewNodeCollecter() *NodesCollector {
	return &NodesCollector{
		cache:   NewAtomicThrottledCache(),
		fetcher: NewCliFetcher("sinfo", "--json"),
		// partition stats
		partitionCpus:        prometheus.NewDesc("slurm_partition_total_cpus", "Total cpus per partition", []string{"partition"}, nil),
		partitionRealMemory:  prometheus.NewDesc("slurm_partition_real_mem", "Real mem per partition", []string{"partition"}, nil),
		partitionFreeMemory:  prometheus.NewDesc("slurm_partition_free_mem", "Free mem per partition", []string{"partition"}, nil),
		partitionAllocMemory: prometheus.NewDesc("slurm_partition_alloc_mem", "Alloc mem per partition", []string{"partition"}, nil),
		partitionAllocCpus:   prometheus.NewDesc("slurm_partition_alloc_cpus", "Alloc cpus per partition", []string{"partition"}, nil),
		partitionIdleCpus:    prometheus.NewDesc("slurm_partition_idle_cpus", "Idle cpus per partition", []string{"partition"}, nil),
		partitionWeight:      prometheus.NewDesc("slurm_partition_weight", "Total node weight per partition??", []string{"partition"}, nil),
		partitionCpuLoad:     prometheus.NewDesc("slurm_partition_cpu_load", "Total cpu load per partition", []string{"partition"}, nil),
		// node cpu summary stats
		totalCpus:     prometheus.NewDesc("slurm_cpus_total", "Total cpus", nil, nil),
		totalIdleCpus: prometheus.NewDesc("slurm_cpus_idle", "Total idle cpus", nil, nil),
		totalCpuLoad:  prometheus.NewDesc("slurm_cpu_load", "Total cpu load", nil, nil),
		cpusPerState:  prometheus.NewDesc("slurm_cpus_per_state", "Cpus per state i.e alloc, mixed, draining, etc.", []string{"state"}, nil),
		// node memory summary stats
		totalRealMemory:  prometheus.NewDesc("slurm_mem_real", "Total real mem", nil, nil),
		totalFreeMemory:  prometheus.NewDesc("slurm_mem_free", "Total free mem", nil, nil),
		totalAllocMemory: prometheus.NewDesc("slurm_mem_alloc", "Total alloc mem", nil, nil),
		// exporter stats
		nodeScrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "slurm_node_scrape_error",
			Help: "slurm node info scrape errors",
		}),
	}
}

func (nc *NodesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- nc.partitionCpus
	ch <- nc.partitionRealMemory
	ch <- nc.partitionFreeMemory
	ch <- nc.partitionAllocMemory
	ch <- nc.partitionAllocCpus
	ch <- nc.partitionIdleCpus
	ch <- nc.partitionWeight
	ch <- nc.partitionCpuLoad
	ch <- nc.totalCpus
	ch <- nc.totalIdleCpus
	ch <- nc.cpusPerState
	ch <- nc.totalRealMemory
	ch <- nc.totalFreeMemory
	ch <- nc.totalAllocMemory
	ch <- nc.nodeScrapeErrors.Desc()
}

func (nc *NodesCollector) Collect(ch chan<- prometheus.Metric) {
	defer func() {
		ch <- nc.nodeScrapeErrors
	}()
	sinfo, err := nc.fetcher.Fetch()
	if err != nil {
		slog.Error("Failed to fetch from cli: " + err.Error())
		nc.nodeScrapeErrors.Inc()
		return
	}
	nodeMetrics, err := parseNodeMetrics(sinfo)
	if err != nil {
		nc.nodeScrapeErrors.Inc()
		slog.Error("Failed to parse node metrics: " + err.Error())
		return
	}
	// partition set
	partitionMetrics := fetchNodePartitionMetrics(nodeMetrics)
	for partition, metric := range partitionMetrics {
		if metric.Cpus > 0 {
			ch <- prometheus.MustNewConstMetric(nc.partitionCpus, prometheus.GaugeValue, metric.Cpus, partition)
		}
		if metric.RealMemory > 0 {
			ch <- prometheus.MustNewConstMetric(nc.partitionRealMemory, prometheus.GaugeValue, metric.RealMemory, partition)
		}
		if metric.FreeMemory > 0 {
			ch <- prometheus.MustNewConstMetric(nc.partitionFreeMemory, prometheus.GaugeValue, metric.FreeMemory, partition)
		}
		if metric.AllocMemory > 0 {
			ch <- prometheus.MustNewConstMetric(nc.partitionAllocMemory, prometheus.GaugeValue, metric.AllocMemory, partition)
		}
		if metric.AllocCpus > 0 {
			ch <- prometheus.MustNewConstMetric(nc.partitionAllocCpus, prometheus.GaugeValue, metric.AllocCpus, partition)
		}
		if metric.IdleCpus > 0 {
			ch <- prometheus.MustNewConstMetric(nc.partitionIdleCpus, prometheus.GaugeValue, metric.IdleCpus, partition)
		}
		if metric.Weight > 0 {
			ch <- prometheus.MustNewConstMetric(nc.partitionWeight, prometheus.GaugeValue, metric.Weight, partition)
		}
		if metric.CpuLoad > 0 {
			ch <- prometheus.MustNewConstMetric(nc.partitionCpuLoad, prometheus.GaugeValue, metric.CpuLoad, partition)
		}
	}
	// node cpu summary set
	nodeCpuMetrics := fetchNodeTotalCpuMetrics(nodeMetrics)
	ch <- prometheus.MustNewConstMetric(nc.totalCpus, prometheus.GaugeValue, nodeCpuMetrics.Total)
	ch <- prometheus.MustNewConstMetric(nc.totalIdleCpus, prometheus.GaugeValue, nodeCpuMetrics.Idle)
	ch <- prometheus.MustNewConstMetric(nc.totalCpuLoad, prometheus.GaugeValue, nodeCpuMetrics.Load)
	for state, cpus := range nodeCpuMetrics.PerState {
		ch <- prometheus.MustNewConstMetric(nc.cpusPerState, prometheus.GaugeValue, cpus, state)
	}
	// node mem summary set
	memMetrics := fetchNodeTotalMemMetrics(nodeMetrics)
	ch <- prometheus.MustNewConstMetric(nc.totalRealMemory, prometheus.GaugeValue, memMetrics.RealMemory)
	ch <- prometheus.MustNewConstMetric(nc.totalFreeMemory, prometheus.GaugeValue, memMetrics.FreeMemory)
	ch <- prometheus.MustNewConstMetric(nc.totalAllocMemory, prometheus.GaugeValue, memMetrics.AllocMemory)
}