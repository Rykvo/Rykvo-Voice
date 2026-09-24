// 仅供测试，交付页面不加载。
ModuleData.items.push(
  ...Array.from({ length: 50 }, (_, index) => {
    const number = index + 1;
    const suffix = String(number).padStart(2, "0");
    const status =
      number === 3 || number % 10 === 0
        ? "error"
        : number === 2 || number % 5 === 0
          ? "offline"
          : "online";
    return {
      id: `module-${suffix}`,
      name: `模块 ${suffix}`,
      number: `+86138${String(number).padStart(8, "0")}`,
      status,
      signal:
        status === "online"
          ? ["mobile", "unicom", "telecom", "wifi"][Math.max(0, index - 3) % 4]
          : "none",
      sims:
        number === 1
          ? [
              { label: "主号", enabled: true },
              { label: "副号", number: "+8613900000002", enabled: false },
            ]
          : [{ label: "主号", enabled: true }],
    };
  }),
);
for (const item of ModuleData.items) {
  if (ModuleData.labels[item.id]) item.label = ModuleData.labels[item.id];
}
