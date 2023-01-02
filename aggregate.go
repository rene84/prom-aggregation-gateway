package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

type metricFamily struct {
	*dto.MetricFamily
	lock sync.RWMutex
}

type aggregate struct {
	familiesLock sync.RWMutex
	families     map[string]*metricFamily
	options      aggregateOptions
}

type ignoredLabels []string

type aggregateOptions struct {
	ignoredLabels     ignoredLabels
	metricTTLDuration *time.Duration
}

type aggregateOptionsFunc func(a *aggregate)

func AddIgnoredLabels(ignoredLabels ...string) aggregateOptionsFunc {
	return func(a *aggregate) {
		a.options.ignoredLabels = ignoredLabels
	}
}

func SetTTLMetricTime(duration *time.Duration) aggregateOptionsFunc {
	return func(a *aggregate) {
		a.options.metricTTLDuration = duration
	}
}

func newAggregate(opts ...aggregateOptionsFunc) *aggregate {
	a := &aggregate{
		families: map[string]*metricFamily{},
		options: aggregateOptions{
			ignoredLabels: []string{},
		},
	}

	for _, opt := range opts {
		opt(a)
	}

	a.options.formatOptions()

	return a
}

func (ao *aggregateOptions) formatOptions() {
	ao.formatIgnoredLabels()
}

func (ao *aggregateOptions) formatIgnoredLabels() {
	if ao.ignoredLabels != nil {
		for i, v := range ao.ignoredLabels {
			ao.ignoredLabels[i] = strings.ToLower(v)
		}
	}

	sort.Strings(ao.ignoredLabels)
}

func (a *aggregate) Len() int {
	a.familiesLock.RLock()
	count := len(a.families)
	a.familiesLock.RUnlock()
	return count
}

// setFamilyOrGetExistingFamily either sets a new family or returns an existing family
func (a *aggregate) setFamilyOrGetExistingFamily(familyName string, family *dto.MetricFamily) *metricFamily {
	a.familiesLock.Lock()
	defer a.familiesLock.Unlock()
	existingFamily, ok := a.families[familyName]
	if !ok {
		a.families[familyName] = &metricFamily{MetricFamily: family}
		return nil
	}
	return existingFamily
}

func (a *aggregate) saveFamily(familyName string, family *dto.MetricFamily) error {
	existingFamily := a.setFamilyOrGetExistingFamily(familyName, family)
	if existingFamily != nil {
		err := existingFamily.mergeFamily(family)
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *aggregate) parseAndMerge(r io.Reader, labels map[string]string) error {
	var parser expfmt.TextParser
	inFamilies, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return err
	}

	for name, family := range inFamilies {
		// Sort labels in case source sends them inconsistently
		for _, m := range family.Metric {
			a.formatLabels(m, labels)
		}

		if err := validateFamily(family); err != nil {
			return err
		}

		// family must be sorted for the merge
		sort.Sort(byLabel(family.Metric))

		if err := a.saveFamily(name, family); err != nil {
			return err
		}

		MetricCountByFamily.WithLabelValues(name).Set(float64(len(family.Metric)))

	}

	TotalFamiliesGauge.Set(float64(a.Len()))

	return nil
}

func (a *aggregate) handleRender(c *gin.Context) {
	contentType := expfmt.Negotiate(c.Request.Header)
	c.Header("Content-Type", string(contentType))
	enc := expfmt.NewEncoder(c.Writer, contentType)

	a.familiesLock.RLock()
	defer a.familiesLock.RUnlock()

	metricNames := []string{}
	metricTypeCounts := make(map[string]int)
	for name, family := range a.families {
		metricNames = append(metricNames, name)
		var typeName string
		if family.Type == nil {
			typeName = "unknown"
		} else {
			typeName = dto.MetricType_name[int32(*family.Type)]
		}
		metricTypeCounts[typeName]++
	}

	sort.Strings(metricNames)

	for _, name := range metricNames {
		if a.encodeMetric(name, enc) {
			return
		}
	}

	MetricCountByType.Reset()
	for typeName, count := range metricTypeCounts {
		MetricCountByType.WithLabelValues(typeName).Set(float64(count))
	}

	// TODO reset gauges
}

func (a *aggregate) encodeMetric(name string, enc expfmt.Encoder) bool {
	a.families[name].lock.RLock()
	defer a.families[name].lock.RUnlock()

	if err := enc.Encode(a.families[name].MetricFamily); err != nil {
		log.Printf("An error has occurred during metrics encoding:\n\n%s\n", err.Error())
		return true
	}
	return false
}

var ErrOddNumberOfLabelParts = errors.New("labels must be defined in pairs")

func (a *aggregate) handleInsert(c *gin.Context) {
	labelParts, err := parseLabelsInPath(c)
	if err != nil {
		log.Println(err)
		http.Error(c.Writer, err.Error(), http.StatusBadRequest)
		return
	}

	labels := make(map[string]string)
	for idx := 0; idx < len(labelParts); idx += 2 {
		name := labelParts[idx]
		value := labelParts[idx+1]
		labels[name] = value
	}

	if err := a.parseAndMerge(c.Request.Body, labels); err != nil {
		log.Println(err)
		http.Error(c.Writer, err.Error(), http.StatusBadRequest)
		return
	}

	job := labels["job"]
	MetricPushes.WithLabelValues(job).Inc()
	c.Status(http.StatusAccepted)
}

func parseLabelsInPath(c *gin.Context) ([]string, error) {
	labelString := c.Param("labels")
	labelString = strings.Trim(labelString, "/")
	if labelString == "" {
		return nil, nil
	}

	labelParts := strings.Split(labelString, "/")
	if len(labelParts)%2 != 0 {
		return nil, ErrOddNumberOfLabelParts
	}

	return labelParts, nil
}
