import json
import logging
import re
from datetime import UTC, datetime
from pathlib import Path
from typing import Literal, TypedDict

import click
import feedparser  # type: ignore

logger = logging.getLogger("state-gov-travel")

_FEED_URL_TEMPLATE = "https://josh.github.io/us-state-travel-advisories-feeds/%s.json"
_FEED_HOMEPAGE_URL = (
    "https://travel.state.gov/content/travel/en/traveladvisories/traveladvisories.html/"
)
_FEED_ICON_URL = (
    "https://travel.state.gov/content/dam/tsg-global/tsg_link_img_display.jpg"
)


class FeedItem(TypedDict):
    id: str
    url: str
    title: str
    content_html: str
    date_published: str


class Feed(TypedDict):
    version: Literal["https://jsonfeed.org/version/1.1"]
    title: str
    home_page_url: str
    feed_url: str
    icon: str
    items: list[FeedItem]


@click.command()
@click.option(
    "--output-dir",
    envvar="OUTPUT_DIR",
    type=click.Path(writable=True, dir_okay=True,
                    file_okay=False, path_type=Path),
    required=True,
)
@click.option("--combine-countries", envvar="COMBINE_COUNTRIES", type=str, default="")
@click.option("--verbose", "-v", is_flag=True)
def main(
    output_dir: Path,
    combine_countries: str,
    verbose: bool,
) -> None:
    log_level = logging.DEBUG if verbose else logging.INFO
    logging.basicConfig(level=log_level)

    url = "https://travel.state.gov/_res/rss/TAsTWs.xml"
    logger.info("Fetch %s", url)
    d: feedparser.FeedParserDict = feedparser.parse(url)

    items: dict[str, FeedItem] = {}

    for entry in d["entries"]:
        link_last_path = entry["link"].split("/")[-1].lower()
        slug = ""
        if link_last_path == "worldwide-caution.html":
            slug = "worldwide"
        elif m := re.match(r"([a-z-]+)-travel-advisory\.html", link_last_path):
            slug = m[1]
        elif m := re.match(r"([a-z-]+)-advisory\.html", link_last_path):
            slug = m[1]
        elif m := re.match(r"([a-z-]+)\.html", link_last_path):
            slug = m[1]
        else:
            logger.warning("Couldn't extract slug from '%s'", entry["link"])
            continue

        title = entry["title"]
        published_datetime = datetime(*entry["published_parsed"][:6]).replace(
            tzinfo=UTC
        )
        published_timestamp: int = int(published_datetime.timestamp())
        guid = f"{slug}-{published_timestamp}"

        item: FeedItem = {
            "id": guid,
            "url": entry["link"],
            "title": title,
            "content_html": entry["summary"],
            "date_published": published_datetime.isoformat(),
        }
        items[slug] = item

    output_dir.mkdir(parents=True, exist_ok=True)
    for output_path in output_dir.glob("*.json"):
        output_path.unlink()

    for slug, item in items.items():
        country = item["title"].split(" - ")[0]
        feed = _feed(
            country=country,
            slug=slug,
            home_page_url=item["url"],
            items=[item],
        )
        output_path = output_dir / f"{slug}.json"
        json.dump(feed, output_path.open("w"), indent=4)

    if combine_countries:
        combine_items: list[FeedItem] = []
        for slug in combine_countries.split(","):
            if slug not in items:
                logger.warning("'%s' not found", slug)
                continue
            combine_items.append(items[slug])
        combine_items.sort(
            key=lambda item: item["date_published"], reverse=True)
        feed = _feed(country="Combined", slug="combined", items=combine_items)
        output_path = output_dir / "combined.json"
        json.dump(feed, output_path.open("w"), indent=4)


def _feed(
    country: str,
    slug: str,
    items: list[FeedItem],
    home_page_url: str = _FEED_HOMEPAGE_URL,
) -> Feed:
    return {
        "version": "https://jsonfeed.org/version/1.1",
        "title": f"U.S. Department of State - {country} Travel Advisories",
        "home_page_url": home_page_url,
        "feed_url": _FEED_URL_TEMPLATE % slug,
        "icon": _FEED_ICON_URL,
        "items": items,
    }


if __name__ == "__main__":
    main()
