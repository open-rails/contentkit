// @vitest-environment jsdom
import "../../test/dom.js";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";
import { createContentKitClient } from "../../client/client.js";
import type { Card, GroupCard } from "../../client/generated/wire.js";
import { ContentKitProvider } from "../../react/provider.js";
import { AdminQuickSearch } from "./quick-search.js";

const at="2026-10-16T00:00:00Z";
const series:GroupCard={id:"series-1",kind:"series",name:"Bleach",slug:"bleach",path:"/series/bleach",internal:false,count:1,attributes:{},created_at:at};
const draft:Card={id:"draft-1",kind:"gallery",code:"DRAFT0001",path:"/g/DRAFT0001/public-path",title:"Bleach draft",language:"en",cover:[],version:{id:"v1",code:"V1",languages:["en"],traits:[],has_audio:false},entities:{},counts:{},access:{allowed:true,level:"public"},attributes:{}};
function server(){
 const requests:URL[]=[];
 const client=createContentKitClient({baseUrl:"https://admin.test",fetch:async(input)=>{
  const url=new URL(String(input));requests.push(url);
  if(url.pathname==="/v1/manage/search-suggestions")return Response.json({contents:[draft],entities:[series]});
  return Response.json({error:"Unexpected public route",code:"forbidden"},{status:403});
 }});
 return{client,requests};
}
const contentHref=(item:Card)=>`/admin/galleries/${item.id}`;
const entityHref=(item:GroupCard)=>`/admin/entities/${item.kind}/${item.id}`;

it("only navigates a chosen result to its actual editor, with no history or public search fallback",async()=>{
 const s=server(),navigate=vi.fn(),user=userEvent.setup();
 render(<ContentKitProvider client={s.client} viewer="staff" navigate={navigate}><AdminQuickSearch contentKinds={["gallery","blog"]} entityKinds={["series","artist"]} contentHref={contentHref} entityHref={entityHref} presentation="header"/></ContentKitProvider>);
 const input=screen.getByRole("combobox",{name:"Search"});await user.type(input,"Bleach");
 await screen.findByRole("option",{name:/Series Bleach/});
 await user.keyboard("{Enter}");expect(navigate).not.toHaveBeenCalled();
 await user.keyboard("{ArrowDown}{ArrowDown}{Enter}");expect(navigate).toHaveBeenCalledWith("/admin/entities/series/series-1");
 expect(s.requests).toHaveLength(1);expect(s.requests[0]?.pathname).toBe("/v1/manage/search-suggestions");
 expect(s.requests[0]?.searchParams.get("include_contents")).toBe("true");
 expect(s.requests[0]?.searchParams.get("include_entities")).toBe("true");
 expect(s.requests[0]?.searchParams.get("content_kinds")).toBe("gallery,blog");
 expect(s.requests[0]?.searchParams.get("entity_kinds")).toBe("series,artist");
});

it("opens draft content through the content editor callback and Escape dismisses the list",async()=>{
 const s=server(),navigate=vi.fn(),user=userEvent.setup();
 render(<ContentKitProvider client={s.client} viewer="staff" navigate={navigate}><AdminQuickSearch contentKinds={["gallery"]} entityKinds={["series"]} contentHref={contentHref} entityHref={entityHref}/></ContentKitProvider>);
 const input=screen.getByRole("combobox",{name:"Search"});await user.type(input,"Bleach");
 await screen.findByRole("option",{name:/Bleach draft/});await user.keyboard("{Escape}");
 expect(input).toHaveAttribute("aria-expanded","false");await user.click(input);
 await user.keyboard("{ArrowDown}{ArrowDown}{Enter}");expect(navigate).toHaveBeenCalledWith("/admin/galleries/draft-1");
});

it("omits targets the host cannot edit and does not use canonical public URLs",async()=>{
 const s=server(),navigate=vi.fn(),user=userEvent.setup();
 render(<ContentKitProvider client={s.client} viewer="moderator" navigate={navigate}><AdminQuickSearch contentKinds={[]} entityKinds={["series"]} contentHref={()=>undefined} entityHref={entityHref}/></ContentKitProvider>);
 await user.type(screen.getByRole("combobox",{name:"Search"}),"Bleach");
 await screen.findByRole("option",{name:/Bleach/});expect(screen.queryByText("Bleach draft")).toBeNull();
 await user.click(screen.getByRole("option",{name:/Bleach/}));expect(navigate).toHaveBeenCalledWith("/admin/entities/series/series-1");
 expect(s.requests[0]?.searchParams.get("include_contents")).toBe("false");
});

it("makes no requests when no editor section is available",async()=>{
 const s=server();render(<AdminQuickSearch client={s.client} contentKinds={[]} entityKinds={[]} contentHref={()=>undefined} entityHref={()=>undefined} defaultValue="Bleach"/>);
 expect(screen.getByRole("combobox",{name:"Search"})).toBeDisabled();
 await new Promise(resolve=>setTimeout(resolve,200));expect(s.requests).toEqual([]);
});

it("clears privileged text and suggestions when the account or allowed target set changes",async()=>{
 const s=server(),user=userEvent.setup();
 const view=(viewer:string,contentKinds:readonly string[])=><ContentKitProvider client={s.client} viewer={viewer}><AdminQuickSearch contentKinds={contentKinds} entityKinds={["series"]} contentHref={contentHref} entityHref={entityHref}/></ContentKitProvider>;
 const page=render(view("old",["gallery"]));const input=screen.getByRole("combobox",{name:"Search"});await user.type(input,"Bleach");await screen.findByRole("option",{name:/Bleach draft/});
 page.rerender(view("new",["gallery"]));expect(screen.getByRole("combobox",{name:"Search"})).toHaveValue("");expect(screen.queryByText("Bleach draft")).toBeNull();
 await user.type(screen.getByRole("combobox",{name:"Search"}),"Bleach");await screen.findByRole("option",{name:/Bleach draft/});
 page.rerender(view("new",[]));await waitFor(()=>expect(screen.getByRole("combobox",{name:"Search"})).toHaveValue(""));expect(screen.queryByText("Bleach draft")).toBeNull();
});
