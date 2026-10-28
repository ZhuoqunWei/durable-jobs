"""Synthetic example, not a replacement for the queue's built-in Go processor."""

from datetime import datetime
import re


def transform(record):
    if set(record) != {"campaign_id", "category", "created_at"}:
        raise ValueError("unexpected campaign fields")
    if not isinstance(record["campaign_id"], str) or not re.fullmatch(
        r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}", record["campaign_id"]
    ):
        raise ValueError("invalid campaign ID")
    categories = {"Pets": "animals", "Medical": "medical", "Education": "education"}
    category = categories[record["category"]]
    date = None
    for layout in ("%m/%d/%Y", "%Y-%m-%d"):
        try:
            candidate = datetime.strptime(record["created_at"], layout)
            # strptime alone accepts some non-padded legacy dates.
            normalized = candidate.date().isoformat()
            if layout == "%m/%d/%Y":
                normalized = f"{candidate.month:02d}/{candidate.day:02d}/{candidate.year:04d}"
            if normalized == record["created_at"]:
                date = candidate
                break
        except ValueError:
            continue
    if date is None:
        raise ValueError("invalid date")
    return {
        "campaign_id": record["campaign_id"],
        "category_id": category,
        "created_date": date.date().isoformat(),
        "schema_version": 1,
    }
