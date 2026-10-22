# syntax=docker/dockerfile:1.7
# SageMaker training image: `training.sagemaker_entry` reads the `train` channel and writes the
# calibrated ONNX model to /opt/ml/model. Pushed to the ECR repository from
# infra/terraform/modules/sagemaker_training; never built or pushed by CI.
FROM python:3.12-slim AS build
ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG NO_PROXY
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never
# Optional extra CA for TLS-intercepting build networks: --secret id=extra_ca,src=<bundle>. Never stored in a layer.
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export PIP_CERT=/run/secrets/extra_ca; fi; \
    pip install --no-cache-dir uv==0.11.3
WORKDIR /app
COPY pyproject.toml uv.lock README.md ./
RUN --mount=type=cache,target=/root/.cache/uv --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export SSL_CERT_FILE=/run/secrets/extra_ca UV_NATIVE_TLS=false; fi; \
    uv sync --frozen --no-default-groups --group training --no-install-project

FROM python:3.12-slim AS runtime
RUN useradd --system --uid 10001 --create-home app && mkdir -p /opt/ml && chown -R 10001 /opt/ml
WORKDIR /app
COPY --from=build /app/.venv /app/.venv
COPY training training
COPY features/spec.yaml features/spec.yaml
ENV PATH=/app/.venv/bin:$PATH PYTHONUNBUFFERED=1 PYTHONPATH=/app
USER 10001
# Batch job, not a server: SageMaker tracks the process exit code instead of a health endpoint.
HEALTHCHECK NONE
ENTRYPOINT ["python", "-m", "training.sagemaker_entry"]
