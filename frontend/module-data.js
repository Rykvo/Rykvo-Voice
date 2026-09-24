const ModuleData = (() => {
  function validLabel(value) {
    const label = value.trim();
    return (
      label.length > 0 && label.length <= 20 && !/[\x00-\x1f\x7f]/.test(label)
    );
  }
  const items = [];
  const labelKey = "rykvo-voice-module-labels-v1";
  const saved = UI.read(labelKey, {});
  const labels = Object.fromEntries(
    Object.entries(
      saved && typeof saved === "object" && !Array.isArray(saved) ? saved : {},
    ).filter(([, label]) => typeof label === "string" && validLabel(label)),
  );
  return { items, labels, labelKey, validLabel };
})();
