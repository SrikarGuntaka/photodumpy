import type { AnchorHTMLAttributes, MouseEvent } from "react";
import { navigate } from "../lib/router";

/**
 * An internal link: client-side navigation for a plain click, and a real
 * <a href> underneath.
 *
 * Only an unmodified left click is intercepted. Middle-click, ctrl/cmd-click
 * and "open in new tab" fall through to the browser, which is what makes them
 * keep working -- a link that hijacks every click breaks all three.
 */
export function Link({ to, onClick, ...rest }: { to: string } & AnchorHTMLAttributes<HTMLAnchorElement>) {
  return (
    <a
      {...rest}
      href={to}
      onClick={(e: MouseEvent<HTMLAnchorElement>) => {
        onClick?.(e);
        if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
        e.preventDefault();
        navigate(to);
      }}
    />
  );
}
