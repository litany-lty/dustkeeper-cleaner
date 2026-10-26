package main

import (
	"context"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/rules"
)

// draftRule is the only place the CLI reaches the model.
//
// The adapter is built from configuration and validated before the first
// request: an unsupported provider, a non-HTTPS endpoint or a missing API key
// fails here with a clear code instead of halfway through a rule.
func (a *app) draftRule(ctx context.Context, text string, allowed map[string]bool) (rules.Draft, error) {
	if a.cfg.Model == nil {
		return rules.Draft{}, errs.New(errs.CodeModelUnavailable,
			"no model is configured; `rule import` is the complete offline path")
	}
	if !a.cfg.Model.Enabled {
		return rules.Draft{}, errs.New(errs.CodeModelUnavailable,
			"the model adapter is disabled in the configuration; use `rule import --file`")
	}
	p, err := rules.NewHTTPProvider(*a.cfg.Model)
	if err != nil {
		return rules.Draft{}, err
	}
	return rules.Compile(ctx, p, text, allowed)
}
