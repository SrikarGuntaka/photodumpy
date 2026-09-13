import { useState } from "react";
import { thumbnailUrl } from "../api/client";

interface ThumbProps {
  photoId: string;
  alt: string;
  /**
   * Whether the API reports a thumbnail exists. False renders a placeholder
   * immediately instead of requesting an image that is known to 404 -- which
   * would otherwise show a broken-image icon for every photo still waiting in
   * the queue.
   */
  available: boolean;
  contain?: boolean;
  className?: string;
}

/**
 * A thumbnail inside a fixed-aspect box.
 *
 * The box, not the image, sets the size. The grid's layout is therefore final
 * before any image has loaded, so tiles do not jump as thumbnails arrive over
 * a slow connection -- and a failed image leaves a labelled placeholder rather
 * than collapsing its tile to zero height.
 */
export function Thumb({ photoId, alt, available, contain, className = "" }: ThumbProps) {
  const [state, setState] = useState<"loading" | "loaded" | "failed">("loading");

  const classes = ["thumb", contain ? "thumb-contain" : "", className].filter(Boolean).join(" ");

  if (!available || state === "failed") {
    return (
      <span className={classes}>
        <span className="thumb-missing">{available ? "Preview unavailable" : "No preview yet"}</span>
      </span>
    );
  }

  return (
    <span className={classes}>
      <img
        src={thumbnailUrl(photoId)}
        alt={alt}
        loading="lazy"
        decoding="async"
        className={state === "loaded" ? "loaded" : ""}
        onLoad={() => setState("loaded")}
        onError={() => setState("failed")}
        draggable={false}
      />
    </span>
  );
}
