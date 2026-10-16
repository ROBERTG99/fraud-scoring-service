# syntax=docker/dockerfile:1.7
# BentoML + onnxruntime model server (port 8514). Loads @production and @shadow from MLflow, falling back
# to the last cached artifact (spec §4.7).
FROM python:3.12-slim AS build
ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG NO_PROXY
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
RUN pip install --no-cache-dir uv==0.11.3
WORKDIR /app
COPY pyproject.toml uv.lock README.md ./
COPY third_party/evalgate/pyproject.toml third_party/evalgate/pyproject.toml
COPY third_party/evalgate/src third_party/evalgate/src
RUN --mount=type=cache,target=/root/.cache/uv uv sync --frozen --no-default-groups --group serving --no-install-project

FROM python:3.12-slim AS runtime
RUN useradd --system --uid 10001 --create-home app && mkdir -p /var/cache/fraud-model && chown 10001 /var/cache/fraud-model
WORKDIR /app
COPY --from=build /app/.venv /app/.venv
COPY serving/__init__.py serving/__init__.py
COPY serving/bentoml serving/bentoml
COPY training/__init__.py training/registry.py training/
ENV PATH=/app/.venv/bin:$PATH PYTHONUNBUFFERED=1 PYTHONPATH=/app FRAUD_MODEL_CACHE=/var/cache/fraud-model \
    BENTOML_HOME=/tmp/bentoml MLFLOW_HTTP_REQUEST_MAX_RETRIES=1 MLFLOW_HTTP_REQUEST_TIMEOUT=5
USER 10001
EXPOSE 8514
HEALTHCHECK --interval=5s --timeout=3s --start-period=30s --retries=20 \
  CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8514/readyz', timeout=2).status == 200 else 1)"]
CMD ["bentoml", "serve", "serving.bentoml.service:FraudModel", "--host", "0.0.0.0", "--port", "8514"]
