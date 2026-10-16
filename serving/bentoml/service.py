"""BentoML model server (port 8514): POST /predict {"inputs": [[13 floats]], "alias": "production|shadow"}."""

from __future__ import annotations

from typing import Any

import bentoml

from serving.bentoml.runtime import ModelNotReadyError, ModelRuntime


@bentoml.service(name="fraud-model-server", traffic={"timeout": 2, "concurrency": 512}, workers=1)
class FraudModel:
    def __init__(self) -> None:
        self.runtime = ModelRuntime.from_env()
        self.status = self.runtime.load_all()

    def __is_ready__(self) -> bool:
        return self.runtime.ready()

    @bentoml.api(route="/predict")  # type: ignore[untyped-decorator]
    def predict(self, inputs: list[list[float]], alias: str = "production") -> dict[str, Any]:
        try:
            return self.runtime.predict(inputs, alias)
        except ModelNotReadyError as exc:
            raise bentoml.exceptions.ServiceUnavailable(str(exc)) from exc
        except ValueError as exc:
            raise bentoml.exceptions.InvalidArgument(str(exc)) from exc

    @bentoml.api(route="/models")  # type: ignore[untyped-decorator]
    def models(self) -> dict[str, Any]:
        return {"loaded": {a: {"version": m.version, "source": m.source} for a, m in self.runtime.models.items()}, "status": self.status}
