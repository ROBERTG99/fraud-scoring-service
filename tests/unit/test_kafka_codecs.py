"""The summarizer must decode every batch codec a franz-go producer may choose (snappy is its default)."""

from aiokafka.codec import has_lz4, has_snappy, has_zstd


def test_consumer_can_decode_go_producer_codecs() -> None:
    assert has_snappy(), "snappy: franz-go's default batch compression"
    assert has_lz4()
    assert has_zstd()
