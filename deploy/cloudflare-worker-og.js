/**
 * Cloudflare Worker: Dynamic OpenGraph Unfurling for PG Cashflow Invites
 * 
 * Deployment:
 * Deploy this worker in your Cloudflare dashboard (or via Wrangler) routed to:
 *   - pg.yourdomain.com/invite/*
 *   - pg.yourdomain.com/?invite=*
 * 
 * Architecture:
 * - Detects social and messaging crawlers (WhatsApp, Telegram, Slack, Twitter/X, iMessage, etc.)
 * - Uses Cloudflare's zero-copy streaming HTMLRewriter API.
 * - For crawler user-agents, fetches public property metadata from /api/join/invite/:code
 *   and injects rich OpenGraph meta tags so WhatsApp/Telegram cards display the actual PG name,
 *   photo, and room allocation details before the user taps.
 * - Standard users stream directly to the origin SPA without latency overhead.
 */

export const BOT_USER_AGENTS = [
  "whatsapp",
  "facebookexternalhit",
  "facebot",
  "twitterbot",
  "telegrambot",
  "slackbot",
  "slack-imgproxy",
  "linkedinbot",
  "discordbot",
  "applebot",
  "bingbot",
  "googlebot",
];

export function isBot(userAgent) {
  if (!userAgent) return false;
  const ua = userAgent.toLowerCase();
  return BOT_USER_AGENTS.some((bot) => ua.includes(bot));
}

export function extractInviteCode(url) {
  const pathname = url.pathname;
  // Case 1: /invite/PG123 or /invite/ROOM-4B
  const match = pathname.match(/^\/invite\/([a-zA-Z0-9_-]+)/);
  if (match) return match[1];

  // Case 2: /?invite=PG123
  const inviteQuery = url.searchParams.get("invite");
  if (inviteQuery) return inviteQuery.trim();

  return null;
}

/**
 * Escapes characters that can break out of HTML attribute contexts.
 * Defends against XSS and attribute injection.
 */
export function escapeHtmlAttr(str) {
  if (typeof str !== "string") return "";
  return str
    .replace(/&/g, "&amp;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

/**
 * Validates and sanitizes image URLs strictly to HTTPS schemes.
 * Rejects javascript:, data:, file:, or unparseable URLs.
 */
export function sanitizeImageUrl(rawUrl, defaultUrl) {
  if (!rawUrl || typeof rawUrl !== "string") return defaultUrl;
  try {
    const parsed = new URL(rawUrl);
    if (parsed.protocol === "https:") {
      return parsed.href;
    }
    return defaultUrl;
  } catch {
    return defaultUrl;
  }
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const userAgent = request.headers.get("user-agent") || "";
    const inviteCode = extractInviteCode(url);

    // If request is not from a crawler or no invite code is present, pass directly through
    if (!inviteCode || !isBot(userAgent)) {
      return fetch(request);
    }

    const defaultImage = `${url.origin}/pwa-512x512.png`;

    // Resolve public property preview from backend API
    let propertyMeta = {
      name: "PG Cashflow",
      details: "Automated rent-due tracking and room allocation.",
      coverImage: defaultImage,
    };

    try {
      const apiOrigin = env?.API_ORIGIN || url.origin;
      const apiResp = await fetch(`${apiOrigin}/api/join/invite/${encodeURIComponent(inviteCode)}`, {
        headers: { "Accept": "application/json" },
        cf: { cacheTtl: 300, cacheEverything: true },
      });

      if (apiResp.ok) {
        const data = await apiResp.json();
        if (data && data.property_name) {
          propertyMeta.name = String(data.property_name);
          propertyMeta.details = `You're invited to join ${data.property_name}. Complete 1-tap Aadhaar KYC and view your room passbook.`;
          if (data.cover_image_url) {
            propertyMeta.coverImage = sanitizeImageUrl(data.cover_image_url, defaultImage);
          }
        }
      }
    } catch (err) {
      // Graceful fallback to default metadata if backend lookup fails
    }

    // Fetch base HTML from origin
    const originResponse = await fetch(request);
    if (!originResponse.headers.get("content-type")?.includes("text/html")) {
      return originResponse;
    }

    const title = `Join ${propertyMeta.name} — Room Allocation`;
    const description = propertyMeta.details;
    const image = propertyMeta.coverImage;

    const safeTitle = escapeHtmlAttr(title);
    const safeDescription = escapeHtmlAttr(description);
    const safeImage = escapeHtmlAttr(image);
    const safeUrl = escapeHtmlAttr(url.href);

    // Stream and rewrite OpenGraph tags dynamically
    return new HTMLRewriter()
      .on("title", {
        element(el) {
          el.setInnerContent(safeTitle);
        },
      })
      .on("meta[property='og:title']", {
        element(el) {
          el.setAttribute("content", safeTitle);
        },
      })
      .on("meta[property='og:description']", {
        element(el) {
          el.setAttribute("content", safeDescription);
        },
      })
      .on("meta[property='og:image']", {
        element(el) {
          el.setAttribute("content", safeImage);
        },
      })
      .on("meta[name='description']", {
        element(el) {
          el.setAttribute("content", safeDescription);
        },
      })
      .on("head", {
        element(el) {
          // Append additional rich preview tags safely escaped
          el.append(`<meta property="og:url" content="${safeUrl}" />`, { html: true });
          el.append(`<meta name="twitter:title" content="${safeTitle}" />`, { html: true });
          el.append(`<meta name="twitter:description" content="${safeDescription}" />`, { html: true });
          el.append(`<meta name="twitter:image" content="${safeImage}" />`, { html: true });
        },
      })
      .transform(originResponse);
  },
};

