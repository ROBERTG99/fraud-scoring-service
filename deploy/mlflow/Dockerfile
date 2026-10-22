# syntax=docker/dockerfile:1.7
# MLflow tracking server + registry. Backend: Postgres database "mlflow"; artifacts served by the server.
FROM python:3.14.7-slim@sha256:51dafde81dbdb6ebde285137a295cf18a47ca95234fe388a343719cb97305b3d AS build
ARG HTTPS_PROXY
ARG HTTP_PROXY
ARG NO_PROXY
RUN pip install --no-cache-dir uv==0.11.3 && uv venv /opt/venv \
 && VIRTUAL_ENV=/opt/venv uv pip install --no-cache "mlflow==3.17.1" "psycopg2-binary==2.9.10"

FROM python:3.14.7-slim@sha256:51dafde81dbdb6ebde285137a295cf18a47ca95234fe388a343719cb97305b3d AS runtime
RUN useradd --system --uid 10001 --create-home mlflow && mkdir -p /mlflow/artifacts && chown -R 10001 /mlflow
COPY --from=build /opt/venv /opt/venv
ENV PATH=/opt/venv/bin:$PATH PYTHONUNBUFFERED=1
USER 10001
EXPOSE 5000
HEALTHCHECK --interval=5s --timeout=3s --start-period=20s --retries=20 \
  CMD ["python", "-c", "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:5000/health', timeout=2).status == 200 else 1)"]
ENTRYPOINT ["mlflow", "server", "--host", "0.0.0.0", "--port", "5000", "--serve-artifacts", "--artifacts-destination", "/mlflow/artifacts"]
# Host headers the stack uses: the service name on the compose network and the loopback port on the host.
CMD ["--backend-store-uri", "postgresql+psycopg2://mlflow:mlflow@postgres:5432/mlflow", \
     "--allowed-hosts", "mlflow,mlflow:5000,localhost,localhost:5000,localhost:8560,127.0.0.1,127.0.0.1:5000,127.0.0.1:8560"]
