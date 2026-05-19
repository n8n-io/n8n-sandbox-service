# End-to-end tests

Playwright drives the HTTP API. Shell scripts start Docker networks, the API, and one or more **sandbox runners** (Docker-in-Docker `n8n-sandbox-service-runner-dind` containers).

Run with `e2e/run-all.sh`.
