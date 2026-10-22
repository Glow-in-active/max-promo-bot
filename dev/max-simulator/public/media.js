const localBannerURL = "https://simulator:4011/assets/banner.png";

export function imageURL(value, origin) {
  if (typeof value !== "string") return null;
  try {
    const url = new URL(value);
    if (url.username || url.password || url.hash || value.includes("#")) return null;
    if (value === localBannerURL) return "/assets/banner.png";
    if (url.protocol === "https:") return url.href;
    if (url.origin === origin && /^\/_content\/[A-Za-z0-9-]+$/.test(url.pathname) && !url.search) return url.href;
    return null;
  } catch {
    return null;
  }
}
