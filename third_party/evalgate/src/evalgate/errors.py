"""Exit codes (as upstream): 0 ok, 1 gate failed, 2 invalid input."""

EXIT_OK = 0
EXIT_GATE_FAILED = 1
EXIT_INVALID_INPUT = 2


class InvalidInputError(Exception):
    """A suite, dataset, report or option the CLI cannot use."""
