const ContactLink = {
  valid(value) {
    if (typeof value !== "string" || value.length > 2048 || /[\s\\<>"\u0000-\u001f\u007f]/u.test(value)) return false;
    if (!value) return true;
    try {
      const url = new URL(value);
      if (url.protocol === "https:" || url.protocol === "http:") {
        return /^https?:\/\//i.test(value) && !!url.hostname && !url.username && !url.password;
      }
      return ["mailto:", "tel:"].includes(url.protocol) && !!url.pathname && !url.host && !url.pathname.startsWith("/");
    } catch { return false; }
  },
};
