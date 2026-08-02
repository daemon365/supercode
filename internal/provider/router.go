package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Route binds a named configured endpoint to its provider implementation.
// An empty Models list makes the route a legacy catch-all route.
type Route struct {
	Name     string
	Models   []string
	Provider Provider
}

// Router dispatches provider-neutral requests by their configured model
// selector. Qualified selectors use the stable "provider/model" form.
type Router struct {
	routes      map[string]Route
	models      []ModelInfo
	unqualified map[string][]ModelInfo
	catchAll    *Route
}

var _ Provider = (*Router)(nil)

func NewRouter(routes []Route) (*Router, error) {
	if len(routes) == 0 {
		return nil, errors.New("at least one provider route is required")
	}
	result := &Router{routes: make(map[string]Route), unqualified: make(map[string][]ModelInfo)}
	for _, route := range routes {
		route.Name = strings.TrimSpace(route.Name)
		if route.Name == "" {
			return nil, errors.New("provider route name is required")
		}
		if strings.Contains(route.Name, "/") {
			return nil, fmt.Errorf("provider route name %q must not contain '/'", route.Name)
		}
		if route.Provider == nil {
			return nil, fmt.Errorf("provider route %q has no implementation", route.Name)
		}
		if _, exists := result.routes[route.Name]; exists {
			return nil, fmt.Errorf("duplicate provider route %q", route.Name)
		}
		result.routes[route.Name] = route
		if len(route.Models) == 0 {
			if result.catchAll != nil {
				return nil, errors.New("only one provider route may omit models")
			}
			copy := route
			result.catchAll = &copy
			continue
		}
		seen := make(map[string]bool)
		for _, model := range route.Models {
			model = strings.TrimSpace(model)
			if model == "" || seen[model] {
				continue
			}
			seen[model] = true
			info := ModelInfo{Selector: ModelSelector(route.Name, model), ID: model, Provider: route.Name}
			result.models = append(result.models, info)
			result.unqualified[model] = append(result.unqualified[model], info)
		}
	}
	return result, nil
}

func ModelSelector(providerName, model string) string {
	return strings.TrimSpace(providerName) + "/" + strings.TrimSpace(model)
}

func (r *Router) Models() []ModelInfo {
	if r == nil {
		return nil
	}
	return append([]ModelInfo(nil), r.models...)
}

func (r *Router) Resolve(selector string) (ModelInfo, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return ModelInfo{}, ErrEmptyModel
	}
	if separator := strings.IndexByte(selector, '/'); separator > 0 {
		name, model := selector[:separator], selector[separator+1:]
		if route, ok := r.routes[name]; ok {
			if strings.TrimSpace(model) == "" {
				return ModelInfo{}, ErrEmptyModel
			}
			if len(route.Models) == 0 || routeContainsModel(route, model) {
				return ModelInfo{Selector: selector, ID: model, Provider: name}, nil
			}
			return ModelInfo{}, fmt.Errorf("model %q is not configured for provider %q", model, name)
		}
	}
	matches := r.unqualified[selector]
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return ModelInfo{}, fmt.Errorf("model %q is configured by multiple providers; use provider/model", selector)
	}
	if r.catchAll != nil {
		return ModelInfo{Selector: selector, ID: selector, Provider: r.catchAll.Name}, nil
	}
	return ModelInfo{}, fmt.Errorf("model %q is not configured by any provider", selector)
}

func (r *Router) ResolveModel(selector string) (ModelInfo, error) { return r.Resolve(selector) }

func routeContainsModel(route Route, model string) bool {
	for _, candidate := range route.Models {
		if strings.TrimSpace(candidate) == model {
			return true
		}
	}
	return false
}

func (r *Router) Generate(ctx context.Context, request Request) (Response, error) {
	implementation, routed, err := r.route(request)
	if err != nil {
		return Response{}, err
	}
	return implementation.Generate(ctx, routed)
}

func (r *Router) Stream(ctx context.Context, request Request) (Stream, error) {
	implementation, routed, err := r.route(request)
	if err != nil {
		return nil, err
	}
	return implementation.Stream(ctx, routed)
}

func (r *Router) route(request Request) (Provider, Request, error) {
	info, err := r.Resolve(request.Model)
	if err != nil {
		return nil, Request{}, err
	}
	route, ok := r.routes[info.Provider]
	if !ok {
		return nil, Request{}, fmt.Errorf("provider %q is not available", info.Provider)
	}
	request.Model = info.ID
	return route.Provider, request, nil
}
