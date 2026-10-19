"""BentoML model server package."""

import os

# No usage tracking: the server must not call out to t.bentoml.com (set before bentoml is imported).
os.environ.setdefault("BENTOML_DO_NOT_TRACK", "true")
