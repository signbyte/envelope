package routes

import (
	"azugo.io/azugo"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
)

// healthz is liveness: the process is up. A probe needs no access line or span.
func (r *router) healthz(ctx *azugo.Context) {
	ctx.SkipRequestLog()
	ctx.StatusCode(fasthttp.StatusOK)
	ctx.JSON(map[string]string{"status": "ok"})
}

// readyz is readiness: the workflow-state store is reachable. It returns a plain
// {status} body — never an error envelope — so an orchestrator probe gets a
// uniform readiness signal, and skips the access line/span.
func (r *router) readyz(ctx *azugo.Context) {
	ctx.SkipRequestLog()
	if err := r.Store().Ping(ctx); err != nil {
		notReady(ctx, "workflow store")

		return
	}
	ctx.StatusCode(fasthttp.StatusOK)
	ctx.JSON(map[string]string{"status": "ready"})
}

// notReady writes the 503 readiness signal (a plain {status} body) and logs the
// unreachable dependency, so an outage is visible in the logs without putting an
// error envelope on a probe response.
func notReady(ctx *azugo.Context, dependency string) {
	ctx.Log().Error("readiness check failed", zap.String("dependency", dependency))
	ctx.StatusCode(fasthttp.StatusServiceUnavailable)
	ctx.JSON(map[string]string{"status": "not_ready"})
}
