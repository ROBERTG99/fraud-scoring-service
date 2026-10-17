"""FakeLLM pattern heuristics: each one encodes a distinction from cmd/gen/PATTERNS.md visible in the facts."""

from __future__ import annotations

from fraud_summarizer.llm.fake import classify

CAP = "43200"


def facts(**features: str) -> dict[str, str]:
    base = {
        "tx_count_10m": "0",
        "tx_count_1h": "0",
        "tx_count_24h": "3",
        "amount_minor": "9000",
        "distinct_devices_24h": "1",
        "new_device": "0",
        "minutes_since_pwd_change": CAP,
        "minutes_since_email_change": CAP,
        "channel": "2",
        "amount_ratio_24h": "1",
    }
    base.update(features)
    out = {f"feature.{k}": v for k, v in base.items()}
    out.update(
        {"decision.action": "REVIEW", "decision.reason_codes": "", "event.transaction.0.device": "newdev"}
    )
    return out


def with_events(f: dict[str, str], **events: str) -> dict[str, str]:
    return {**f, **{"event." + k.replace("__", "."): v for k, v in events.items()}}


def test_login_from_an_unbound_new_device_is_a_quiet_takeover() -> None:
    f = with_events(
        facts(new_device="1", amount_ratio_24h="3.2"), login__0__device="newdev", login__0__success="true"
    )
    assert classify(f) == "account_takeover"


def test_a_bound_new_phone_without_credential_change_stays_unclear() -> None:
    f = with_events(
        facts(new_device="1", amount_ratio_24h="3.2"),
        device__0__change="device_bound",
        device__0__device="newdev",
        login__0__device="newdev",
        login__0__method="otp",
    )
    assert classify(f) == "unclear"


def test_a_reset_and_a_bound_phone_with_an_ordinary_amount_is_not_a_takeover() -> None:
    f = with_events(
        facts(new_device="1", minutes_since_pwd_change="12", amount_ratio_24h="1.1"),
        device__0__change="device_bound",
        device__1__change="password_changed",
    )
    f["decision.reason_codes"] = "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE"
    assert classify(f) == "unclear"
    loud = dict(f, **{"feature.amount_ratio_24h": "14"})
    loud.pop("event.device.0.change")
    assert classify(loud) == "account_takeover"


def test_a_burst_from_a_new_device_is_card_testing_even_before_a_large_purchase() -> None:
    assert classify(facts(new_device="1", tx_count_10m="9", amount_minor="18000")) == "card_testing"


def test_small_repeated_purchases_on_the_home_device_are_not_card_testing() -> None:
    f = facts(new_device="0", tx_count_10m="9", tx_count_1h="9", amount_minor="250")
    f["decision.reason_codes"] = "CARD_TESTING_BURST"
    assert classify(f) == "unclear"


def test_slow_small_tests_from_a_new_device_are_card_testing() -> None:
    assert (
        classify(facts(new_device="1", tx_count_10m="1", tx_count_1h="5", amount_minor="600"))
        == "card_testing"
    )


def test_p2p_from_a_freshly_bound_device_at_mule_amounts_is_a_mule() -> None:
    f = with_events(
        facts(channel="3", distinct_devices_24h="2", amount_minor="84000"), device__0__change="device_bound"
    )
    assert classify(f) == "mule_account"
    split_bill = with_events(
        facts(channel="3", distinct_devices_24h="2", amount_minor="4500"), device__0__change="device_bound"
    )
    assert classify(split_bill) == "unclear"
