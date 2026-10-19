"""Export the GBDT plus isotonic calibration as one ONNX graph.

Graph: input `features` FP32 [n, 13] → tree ensemble → P(fraud) → isotonic piecewise-linear map →
output `probability` FP32 [n, 1]. The isotonic step is written with plain ONNX ops so every runtime
(onnxruntime in BentoML, Triton's ONNX Runtime backend) produces the calibrated score.
"""

from __future__ import annotations

from typing import Any

import numpy as np
import numpy.typing as npt
import onnx
from onnx import TensorProto, helper, numpy_helper

from training.features import spec_version

N_FEATURES = 13
INPUT = "features"
OUTPUT = "probability"


def _tree_model(model: Any, backend: str) -> onnx.ModelProto:
    if backend == "xgboost":
        from onnxmltools.convert import convert_xgboost
        from onnxmltools.convert.common.data_types import FloatTensorType

        converted: onnx.ModelProto = convert_xgboost(
            model, initial_types=[(INPUT, FloatTensorType([None, N_FEATURES]))], target_opset=15
        )
        return converted
    from skl2onnx import convert_sklearn
    from skl2onnx.common.data_types import FloatTensorType as SkFloat

    return convert_sklearn(  # type: ignore[no-any-return]
        model,
        initial_types=[(INPUT, SkFloat([None, N_FEATURES]))],
        target_opset={"": 15, "ai.onnx.ml": 3},
        options={id(model): {"zipmap": False}},
    )


def _probabilities_output(m: onnx.ModelProto) -> str:
    for out in m.graph.output:
        if out.name in {"probabilities", "probability_tensor", "output_probability"}:
            return str(out.name)
    return str(m.graph.output[-1].name)


def isotonic_nodes(
    x_thresholds: npt.NDArray[np.float64], y_thresholds: npt.NDArray[np.float64], src: str
) -> tuple[list[onnx.NodeProto], list[onnx.TensorProto]]:
    """Nodes computing np.interp(p, X, Y) for p = src[:, 1] as y0 + Σ slope_i · clip(p − x_i, 0, Δx_i)."""
    x = np.asarray(x_thresholds, dtype=np.float64)
    y = np.asarray(y_thresholds, dtype=np.float64)
    dx = np.diff(x)
    keep = dx > 0
    slopes = np.zeros_like(dx)
    slopes[keep] = np.diff(y)[keep] / dx[keep]
    inits = [
        numpy_helper.from_array(np.array([1], dtype=np.int64), "iso_col_start"),
        numpy_helper.from_array(np.array([2], dtype=np.int64), "iso_col_end"),
        numpy_helper.from_array(np.array([1], dtype=np.int64), "iso_axes"),
        numpy_helper.from_array(x[:-1][keep].astype(np.float32).reshape(1, -1), "iso_x"),
        numpy_helper.from_array(dx[keep].astype(np.float32).reshape(1, -1), "iso_dx"),
        numpy_helper.from_array(slopes[keep].astype(np.float32).reshape(1, -1), "iso_slope"),
        numpy_helper.from_array(np.array([y[0]], dtype=np.float32), "iso_y0"),
        numpy_helper.from_array(np.array(0.0, dtype=np.float32), "iso_zero"),
        numpy_helper.from_array(np.array([1], dtype=np.int64), "iso_sum_axes"),
        numpy_helper.from_array(np.array(0.0, dtype=np.float32), "iso_min"),
        numpy_helper.from_array(np.array(1.0, dtype=np.float32), "iso_max"),
    ]
    nodes = [
        helper.make_node("Slice", [src, "iso_col_start", "iso_col_end", "iso_axes"], ["iso_p"]),  # [n,1]
        helper.make_node("Sub", ["iso_p", "iso_x"], ["iso_d"]),  # [n,k]
        helper.make_node("Max", ["iso_d", "iso_zero"], ["iso_d0"]),
        helper.make_node("Min", ["iso_d0", "iso_dx"], ["iso_dc"]),
        helper.make_node("Mul", ["iso_dc", "iso_slope"], ["iso_terms"]),
        helper.make_node("ReduceSum", ["iso_terms", "iso_sum_axes"], ["iso_sum"], keepdims=1),  # [n,1]
        helper.make_node("Add", ["iso_sum", "iso_y0"], ["iso_raw"]),
        helper.make_node("Clip", ["iso_raw", "iso_min", "iso_max"], [OUTPUT]),
    ]
    return nodes, inits


def export(
    model: Any, backend: str, iso_x: npt.NDArray[np.float64], iso_y: npt.NDArray[np.float64], version: str
) -> onnx.ModelProto:
    m = _tree_model(model, backend)
    src = _probabilities_output(m)
    nodes, inits = isotonic_nodes(iso_x, iso_y, src)
    if not any(op.domain in ("", "ai.onnx") for op in m.opset_import):
        m.opset_import.append(helper.make_opsetid("", 15))
    graph = m.graph
    graph.node.extend(nodes)
    graph.initializer.extend(inits)
    del graph.output[:]
    graph.output.append(helper.make_tensor_value_info(OUTPUT, TensorProto.FLOAT, [None, 1]))
    graph.name = "fraud_gbdt"
    m.doc_string = f"fraud-gbdt {version}: {backend} + isotonic calibration, feature spec v1"
    del m.metadata_props[:]
    for k, v in {"model_version": version, "feature_spec": spec_version(), "backend": backend}.items():
        m.metadata_props.append(onnx.StringStringEntryProto(key=k, value=v))
    onnx.checker.check_model(m)
    return m


def run_onnx(model_bytes: bytes, x: npt.NDArray[np.float32]) -> npt.NDArray[np.float32]:
    import onnxruntime as ort

    opts = ort.SessionOptions()
    opts.intra_op_num_threads = 1
    sess = ort.InferenceSession(model_bytes, sess_options=opts, providers=["CPUExecutionProvider"])
    out = sess.run([OUTPUT], {INPUT: x.astype(np.float32)})[0]
    return np.asarray(out, dtype=np.float32).reshape(-1)
