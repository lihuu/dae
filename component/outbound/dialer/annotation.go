/*
*  SPDX-License-Identifier: AGPL-3.0-only
*  Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package dialer

import (
	"fmt"
	"strconv"
	"time"

	"github.com/daeuniverse/dae/pkg/config_parser"
)

const (
	AnnotationKey_AddLatency = "add_latency"
	AnnotationKey_Priority   = "priority"
)

// PriorityNotSet is a sentinel indicating no priority annotation was provided.
const PriorityNotSet = -1

type Annotation struct {
	AddLatency time.Duration
	// Priority is the integer priority annotation for failover policy.
	// 0 = primary, 1 = fallback. PriorityNotSet means no annotation.
	Priority int
}

func NewAnnotation(annotation []*config_parser.Param) (*Annotation, error) {
	anno := Annotation{
		Priority: PriorityNotSet,
	}
	for _, param := range annotation {
		switch param.Key {
		case AnnotationKey_AddLatency:
			latency, err := time.ParseDuration(param.Val)
			if err != nil {
				return nil, fmt.Errorf("incorrect latency format: %w", err)
			}
			// Only the first setting is valid.
			if anno.AddLatency == 0 {
				anno.AddLatency = latency
			}
		case AnnotationKey_Priority:
			p, err := strconv.Atoi(param.Val)
			if err != nil {
				return nil, fmt.Errorf("incorrect priority format: %w", err)
			}
			// Only the first setting is valid.
			if anno.Priority == PriorityNotSet {
				anno.Priority = p
			}
		default:
			return nil, fmt.Errorf("unknown filter annotation: %v", param.Key)
		}
	}
	return &anno, nil
}
