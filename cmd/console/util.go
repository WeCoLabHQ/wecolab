package main

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func parseDuration(s string) (metav1.Duration, error) {
	if s == "" {
		return metav1.Duration{Duration: 5 * time.Minute}, nil
	}
	d, err := time.ParseDuration(s)
	return metav1.Duration{Duration: d}, err
}
