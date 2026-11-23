import { describe, expect, it } from "vitest";

import { objectLabel, objectName, typeLabel } from "./vocab";

describe("object labels", () => {
  it("reads a default-privilege entry as its role, object type and schema", () => {
    const scoped = { objectType: "defaultPrivilege", schema: "app", name: "app_owner/tables" };
    const wide = { objectType: "defaultPrivilege", name: "app_owner/functions" };
    expect(objectName(scoped)).toBe("app_owner on tables in app");
    expect(objectLabel(scoped)).toBe("default privileges app_owner on tables in app");
    expect(objectName(wide)).toBe("app_owner on functions");
    expect(typeLabel("defaultPrivilege")).toBe("default privileges");
  });

  it("leaves every other object as type and qualified name", () => {
    expect(objectLabel({ objectType: "table", schema: "sales", name: "regions" })).toBe("table sales.regions");
    expect(objectName({ name: "app" })).toBe("app");
  });
});
